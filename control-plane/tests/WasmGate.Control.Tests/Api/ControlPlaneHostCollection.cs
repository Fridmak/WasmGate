namespace WasmGate.Control.Tests.Api;

/// <summary>
/// WebApplicationFactory tests mutate process-wide WASMGATE_CP_* environment
/// variables while the composition root starts, so they must not overlap.
/// </summary>
[CollectionDefinition("Control-plane host", DisableParallelization = true)]
public sealed class ControlPlaneHostCollection;
