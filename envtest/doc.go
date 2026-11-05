// Package envtest is the chartered test-only surface of
// environment-services: fakes for the public interfaces consumers
// program against (environments.Observation, envspec.ServiceProvider)
// so consumer tests can run without a manager, a Docker daemon or a
// device.
//
// Charter: production code must never import this package. It ships
// fakes of consumer-facing interfaces only — deliberately no
// collector-authoring API and no fake adapter surface. Tests that
// need recorded signal data publish envspec.SignalEvent values (or
// decode golden captures) through FakeObservation.Publish.
package envtest
