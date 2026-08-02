package kafka

import (
	"github.com/twmb/franz-go/pkg/kgo"
	"github.com/twmb/franz-go/pkg/sasl"
	"github.com/twmb/franz-go/pkg/sasl/plain"
	"github.com/twmb/franz-go/pkg/sasl/scram"
)

// Mechanism is a Warren-owned SASL mechanism. The zero value is invalid;
// build one with Plain, SCRAM256 or SCRAM512.
//
// It exists so a service configuring authentication does not import franz-go:
// invariant 3 keeps driver types out of Warren signatures, and these three
// cover essentially all managed Kafka. Anything else goes through RawSASL,
// which is the named carve-out.
type Mechanism struct {
	kind       mechKind
	user, pass string
}

type mechKind uint8

const (
	mechNone mechKind = iota
	mechPlain
	mechSCRAM256
	mechSCRAM512
)

// Plain returns the SASL/PLAIN mechanism.
//
// Use it only with TLS: PLAIN sends the credentials in the clear, and the
// boot refuses the combination rather than let a password cross an
// unencrypted network because someone forgot an option.
func Plain(user, pass string) Mechanism {
	return Mechanism{kind: mechPlain, user: user, pass: pass}
}

// SCRAM256 returns the SASL/SCRAM-SHA-256 mechanism.
func SCRAM256(user, pass string) Mechanism {
	return Mechanism{kind: mechSCRAM256, user: user, pass: pass}
}

// SCRAM512 returns the SASL/SCRAM-SHA-512 mechanism.
func SCRAM512(user, pass string) Mechanism {
	return Mechanism{kind: mechSCRAM512, user: user, pass: pass}
}

// SASL sets the authentication mechanism.
func SASL(m Mechanism) Option {
	return Option{apply: func(c *config) { c.mechanism = m }}
}

// RawSASL sets a franz-go mechanism directly. It is a named escape hatch
// (AGENT.md invariant 3) for the mechanisms Warren does not model —
// OAUTHBEARER's token callback, AWS MSK IAM, Kerberos:
//
//	kafka.RawSASL(oauth.Oauth(tokenSource))
//
// Everything else should use SASL, which keeps franz-go out of the calling
// service's imports.
func RawSASL(m sasl.Mechanism) Option {
	return Option{apply: func(c *config) { c.rawSASL = m }}
}

// resolve turns the Warren mechanism into franz-go's. An invalid kind is
// unreachable: the constructor validates before this runs.
func (m Mechanism) resolve() sasl.Mechanism {
	switch m.kind {
	case mechPlain:
		return plain.Auth{User: m.user, Pass: m.pass}.AsMechanism()
	case mechSCRAM256:
		return scram.Auth{User: m.user, Pass: m.pass}.AsSha256Mechanism()
	case mechSCRAM512:
		return scram.Auth{User: m.user, Pass: m.pass}.AsSha512Mechanism()
	default:
		return nil
	}
}

// Balancer is the consumer group's partition-assignment strategy.
type Balancer string

const (
	// BalancerCooperative is the default: cooperative-sticky assignment,
	// which revokes only the partitions that actually move — a stop-the-world
	// rebalance fights the drain.
	//
	// It CANNOT coexist with the eager balancers in one group. Changing this
	// setting requires the whole group restarted, not a rolling deploy.
	BalancerCooperative Balancer = "cooperative"
	// BalancerSticky is eager sticky assignment.
	BalancerSticky Balancer = "sticky"
	// BalancerRoundRobin is eager round-robin assignment.
	BalancerRoundRobin Balancer = "roundrobin"
	// BalancerRange is eager range assignment — what a group shared with a
	// Java consumer on defaults is using.
	BalancerRange Balancer = "range"
)

func (b Balancer) valid() bool {
	switch b {
	case BalancerCooperative, BalancerSticky, BalancerRoundRobin, BalancerRange:
		return true
	}
	return false
}

func (b Balancer) resolve() kgo.GroupBalancer {
	switch b {
	case BalancerSticky:
		return kgo.StickyBalancer()
	case BalancerRoundRobin:
		return kgo.RoundRobinBalancer()
	case BalancerRange:
		return kgo.RangeBalancer()
	default:
		return kgo.CooperativeStickyBalancer()
	}
}
