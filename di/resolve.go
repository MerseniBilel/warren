package di

import "reflect"

// Resolve assigns the instance of T to target. It reports an error if T has no
// provider, if the container is not built, or if T is a group.
//
// After [Container.Build] this is a map lookup and a type assertion: no
// construction, no reflection beyond the type key, and no allocation.
//
// The out-parameter is deliberate: returning (T, error) would have to materialise
// a zero T on the error path. See SPEC.md §11.3.
func Resolve[T any](c *Container, target *T) error {
	err := c.resolvable(opResolve)
	if err != nil {
		return err
	}

	want := reflect.TypeFor[T]()

	value, ok := c.instances[want]
	if !ok {
		if len(c.grouped[want]) > 0 {
			return errIsGroup(want)
		}

		return errNoProvider(opResolve, want, nil, Site{}, c.nearMiss(want))
	}

	instance, ok := value.(T)
	if !ok {
		return errNoProvider(opResolve, want, nil, Site{}, nil)
	}

	*target = instance

	return nil
}

// Group assigns every contributed instance of T to target, in registration
// order — which is module registration order.
//
// That order is deliberate and tested: a middleware chain or a route table whose
// order varies between runs is a bug that reproduces once a week. A type nobody
// contributed yields an empty slice and no error, because "no members" is a
// legitimate answer.
func Group[T any](c *Container, target *[]T) error {
	err := c.resolvable(opGroup)
	if err != nil {
		return err
	}

	want := reflect.TypeFor[T]()

	value, ok := c.groups[want]
	if !ok {
		if _, single := c.single[want]; single {
			return errIsSingle(want)
		}

		*target = nil

		return nil
	}

	members, ok := value.([]T)
	if !ok {
		return errNoProvider(opGroup, want, nil, Site{}, nil)
	}

	*target = members

	return nil
}

// resolvable reports why the container cannot answer a resolution yet.
func (c *Container) resolvable(op string) error {
	if !c.built {
		return errNotBuilt(op)
	}

	return c.buildErr
}
