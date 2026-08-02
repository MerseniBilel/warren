package observability

import "fmt"

// diagnostic carries a rendered multi-line block; the text is the contract,
// covered by golden files like every other Warren diagnostic.
type diagnostic string

func (d diagnostic) Error() string { return string(d) }

func errBadSampleRatio(r float64) error {
	return diagnostic(fmt.Sprintf(
		"✗ sample ratio out of range\n\n    observability.SampleRatio(%v)\n\n"+
			"  It is a probability: 0 records nothing, 1 records everything.\n"+
			"  This fails the boot rather than request one, because a ratio\n"+
			"  nobody noticed was wrong is a month of traces you do not have.",
		r))
}

func errNoServiceName() error {
	return diagnostic(
		"✗ telemetry has no service name\n\n" +
			"    observability.Module() has an OTLPEndpoint but no ServiceName.\n\n" +
			"  An OTLP resource with no service.name is unroutable: the collector\n" +
			"  accepts the spans and nothing can find them again. Add:\n\n" +
			"      observability.ServiceName(\"user-service\")")
}

func errCannotExport(endpoint string, cause error) error {
	return diagnostic(fmt.Sprintf(
		"✗ cannot reach the OTLP collector\n\n    %s\n    %v\n\n"+
			"  The endpoint is host:port for OTLP over gRPC — no scheme, no path.\n"+
			"  A collector on the cluster network usually also needs:\n\n"+
			"      observability.Insecure()\n\n"+
			"  To run without a collector at all, leave OTLPEndpoint empty: that\n"+
			"  disables export and costs the request path nothing.",
		endpoint, cause))
}
