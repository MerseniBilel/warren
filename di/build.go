package di

import (
	"context"
	"reflect"
)

// Build validates the graph and then constructs every singleton, in dependency
// order. It is idempotent: a second call returns the first call's error and
// constructs nothing.
//
// ctx is the boot context. A constructor may take a [context.Context] parameter
// to receive it — for a driver whose own constructor requires one — and must not
// retain it: it is cancelled when boot finishes, not when the process stops.
//
// Constructing in dependency order is what makes warren/lifecycle's reverse-order
// shutdown correct: a hook registered while a constructor runs is already in
// dependency order, so the pool that was built before the broker is stopped after
// it.
//
// A failed Build leaves whatever it had already constructed. Nothing is released:
// the caller is warren.Run, which is about to exit, and a teardown path running
// against a half-built graph is a second, less-tested failure mode.
func (c *Container) Build(ctx context.Context) error {
	if c.built {
		return c.buildErr
	}

	c.built = true

	if problems := c.validate(opBuild); len(problems) > 0 {
		c.buildErr = join(problems)

		return c.buildErr
	}

	for _, p := range c.order {
		_, err := c.instance(ctx, p)
		if err != nil {
			c.buildErr = err

			return err
		}
	}

	c.buildGroups()

	return nil
}

// instance returns p's value, constructing it if this is the first time it is
// needed.
//
// Memoisation is by constructor identity, not by the type registered, so one
// constructor registered against two ports yields one instance. See memoID.
func (c *Container) instance(ctx context.Context, p *provider) (any, error) {
	if v, ok := c.made[p]; ok {
		return v, nil
	}

	if p.kind == KindSupplied {
		c.remember(p, p.value)

		return p.value, nil
	}

	if p.id != 0 {
		if v, ok := c.byCtor[p.id]; ok {
			c.remember(p, v)

			return v, nil
		}
	}

	args, err := c.args(ctx, p)
	if err != nil {
		return nil, err
	}

	value, err := c.call(p, args)
	if err != nil {
		return nil, err
	}

	if isNil(reflect.ValueOf(value)) {
		return nil, errReturnedNil(p)
	}

	c.remember(p, value)

	return value, nil
}

// remember files a constructed value against its provider, its constructor, and
// — for anything but a group member — the type it was registered as.
func (c *Container) remember(p *provider, value any) {
	c.made[p] = value

	if p.id != 0 {
		c.byCtor[p.id] = value
	}

	if p.kind == KindContributed {
		return
	}

	if _, exists := c.instances[p.typ]; !exists {
		c.instances[p.typ] = value
	}
}

// args resolves a constructor's parameters, in declaration order.
func (c *Container) args(ctx context.Context, p *provider) ([]reflect.Value, error) {
	args := make([]reflect.Value, 0, len(p.deps))

	for _, dep := range p.deps {
		if dep == contextType() {
			args = append(args, reflect.ValueOf(ctx))

			continue
		}

		if single, ok := c.single[dep]; ok {
			value, err := c.instance(ctx, single)
			if err != nil {
				return nil, err
			}

			args = append(args, reflect.ValueOf(value))

			continue
		}

		group, err := c.groupValue(ctx, dep)
		if err != nil {
			return nil, err
		}

		args = append(args, group)
	}

	return args, nil
}

// groupValue builds the slice a constructor asked for from the group's members.
// Validation has already established that the group exists.
func (c *Container) groupValue(ctx context.Context, dep reflect.Type) (reflect.Value, error) {
	members := c.grouped[dep.Elem()]
	slice := reflect.MakeSlice(dep, 0, len(members))

	for _, member := range members {
		value, err := c.instance(ctx, member)
		if err != nil {
			return reflect.Value{}, err
		}

		slice = reflect.Append(slice, reflect.ValueOf(value))
	}

	return slice, nil
}

// buildGroups materialises each group as a []T once, so that Group is a map
// lookup rather than a slice built per call.
func (c *Container) buildGroups() {
	for _, p := range c.order {
		if p.kind != KindContributed {
			continue
		}

		if _, done := c.groups[p.typ]; done {
			continue
		}

		members := c.grouped[p.typ]
		slice := reflect.MakeSlice(reflect.SliceOf(p.typ), 0, len(members))

		for _, member := range members {
			slice = reflect.Append(slice, reflect.ValueOf(c.made[member]))
		}

		c.groups[p.typ] = slice.Interface()
	}
}

// call invokes a constructor, converting both a returned error and a panic into
// a message that names the constructor.
func (c *Container) call(p *provider, args []reflect.Value) (value any, err error) {
	// A panic in a user's constructor would otherwise surface as a stack full of
	// reflect frames, naming nothing the reader can act on. See SPEC.md §11.5.
	defer func() {
		if recovered := recover(); recovered != nil {
			value, err = nil, errPanicked(p, recovered)
		}
	}()

	results := p.ctor.Call(args)

	if len(results) == valueAndError && !results[1].IsNil() {
		cause, isError := results[1].Interface().(error)
		if !isError {
			// Unreachable: bindReturns established that the second return is an
			// error before this constructor was ever accepted.
			return nil, errPanicked(p, "constructor returned a non-error second value")
		}

		return nil, errConstructorFailed(p, cause)
	}

	return results[0].Interface(), nil
}
