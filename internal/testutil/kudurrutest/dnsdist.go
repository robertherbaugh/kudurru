package kudurrutest

// DnsdistImage is the dnsdist program that the opt-in live edge suites run
// in a private Docker daemon (TestConsoleLiveSupervisor,
// TestManagedCacheLivePinnedTransition, TestRunServeRemoteEdge*,
// TestRunServeManagedEdgePinnedDaemonRestart and
// TestRunServeUniFiHostnamePolicyLifecycle). It is the upstream image of the
// FROM line in images/dnsdist/Dockerfile, by registry digest, so any machine
// can pull the same bytes (D383). The suites start it with --entrypoint
// dnsdist and never use the kudurru supervisor of the release image.
// TestSupervisorVersions (deploy/release_test.go) fails when Dependabot
// moves the FROM line and this constant stays behind.
const DnsdistImage = "powerdns/dnsdist-21:2.1.3@sha256:c70ae7363712188ab2878d22bc9f1801cae29190688474a224caee2089f74837"
