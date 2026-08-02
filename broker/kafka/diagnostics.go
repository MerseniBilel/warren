package kafka

import (
	"fmt"
	"strings"
)

// diagnostic carries a rendered multi-line block; the text is the contract,
// covered by golden files like every other Warren diagnostic.
type diagnostic string

func (d diagnostic) Error() string { return string(d) }

func errNoBrokers() error {
	return diagnostic(
		"✗ kafka has no seed brokers\n\n" +
			"    kafka.Broker() was declared without kafka.Brokers(...).\n\n" +
			"  Give it at least one, usually from config:\n\n" +
			"      kafka.Broker(kafka.Brokers(cfg.Kafka.Brokers...))\n\n" +
			"  Seeds are only a starting point — the client discovers the rest of\n" +
			"  the cluster from them, so listing two or three is enough.")
}

func errNoConsumerGroup(topic string) error {
	return diagnostic(fmt.Sprintf(
		"✗ kafka has no consumer group\n\n"+
			"    A subscription to %q needs one, and kafka.Broker() was declared\n"+
			"    without kafka.ConsumerGroup(...).\n\n"+
			"  The group is what makes partitions divide between your replicas\n"+
			"  instead of every replica reading everything:\n\n"+
			"      kafka.Broker(\n"+
			"          kafka.Brokers(...),\n"+
			"          kafka.ConsumerGroup(\"billing-service\"),\n"+
			"      )\n\n"+
			"  A service that only publishes needs no group.", topic))
}

func errUnknownBalancer(b Balancer) error {
	return diagnostic(fmt.Sprintf(
		"✗ unknown partition assignment %q\n\n    kafka.PartitionAssignment(%q)\n\n"+
			"  Available: cooperative (the default), sticky, roundrobin, range.\n\n"+
			"  Cooperative revokes only the partitions that actually move, so a\n"+
			"  rebalance does not stop every consumer in the group. Note it cannot\n"+
			"  coexist with the eager balancers in one group: changing this needs\n"+
			"  the whole group restarted, not a rolling deploy.", string(b), string(b)))
}

func errPlainWithoutTLS() error {
	return diagnostic(
		"✗ SASL/PLAIN without TLS\n\n" +
			"    kafka.SASL(kafka.Plain(...)) was set with no kafka.TLS(...).\n\n" +
			"  PLAIN sends the username and password in the CLEAR. This fails the\n" +
			"  boot rather than let a credential cross an unencrypted network\n" +
			"  because an option was forgotten.\n\n" +
			"  Add TLS:\n\n" +
			"      kafka.TLS(&tls.Config{MinVersion: tls.VersionTLS12})\n\n" +
			"  or use kafka.SCRAM512(...), which never sends the password itself.")
}

func errTwoMechanisms() error {
	return diagnostic(
		"✗ two SASL mechanisms\n\n" +
			"    Both kafka.SASL(...) and kafka.RawSASL(...) were set.\n\n" +
			"  Only one authenticates the connection, and which one would win is\n" +
			"  not something to leave to argument order. Keep kafka.SASL for the\n" +
			"  mechanisms Warren models — Plain, SCRAM256, SCRAM512 — and\n" +
			"  kafka.RawSASL only for the ones it does not, such as OAUTHBEARER\n" +
			"  or AWS MSK IAM.")
}

func errCannotConnect(brokers []string, cause error) error {
	return diagnostic(fmt.Sprintf(
		"✗ cannot reach kafka\n\n    %s\n    %v\n\n"+
			"  The seeds parsed, so this is the cluster, the network, or the\n"+
			"  credentials. A SASL failure looks the same as an unreachable broker\n"+
			"  from here — check both.\n\n"+
			"  This fails the boot deliberately: a service that cannot reach its\n"+
			"  broker cannot serve, and discovering that on the first publish is\n"+
			"  worse than discovering it now.",
		strings.Join(brokers, ", "), cause))
}

func errRawFailed(cause error) error {
	return diagnostic(fmt.Sprintf(
		"✗ kafka.Raw failed\n\n    %v\n\n"+
			"  The client connected and the function passed to kafka.Raw returned\n"+
			"  an error, so the boot is abandoned and the client closed.", cause))
}

func errNotStarted() error {
	return diagnostic(
		"✗ kafka client used before it was started\n\n" +
			"    The client is opened by this module's OnStart hook, at boot step 6.\n\n" +
			"  Something published or subscribed during CONSTRUCTION instead. A\n" +
			"  constructor wires; it does not acquire. Move the call into an\n" +
			"  OnStart hook of your own, which runs after this one.")
}
