using Microsoft.AspNetCore.Server.Kestrel.Core;
using WasmGate.Control.Api;
using WasmGate.Control.Api.Endpoints;
using WasmGate.Control.Api.Endpoints.Admin;
using WasmGate.Control.Api.Grpc;
using WasmGate.Control.Api.Hosting;
using WasmGate.Control.Api.Infrastructure.Networking;
using WasmGate.Control.Api.Infrastructure.Security;
using WasmGate.Control.Api.Observability;
using WasmGate.Control.Core.Abstractions.Releases;
using WasmGate.Control.Infrastructure.Storage;
using AdminJsonContext = WasmGate.Control.Api.Contracts.AdminJsonContext;

// Composition root of the control plane: options, logging, persistence, gRPC
// services and the admin HTTP API. Everything is registered as a singleton,
// because the repositories are stateless over one connection pool and the
// services only coordinate them.
//
// The content root is the application directory, not the working directory: the
// admin UI lives in wwwroot next to the binary and must be found regardless of
// where the service was started.
var options = IHostApplicationBuilder.GetCpOptions();

// A tag/release build can copy the database without starting the listeners.
if (args.Length > 0 && args[0] == "--backup")
{
    if (args.Length != 2)
    {
        Console.Error.WriteLine("usage: wasmgate-control --backup <path>");
        Environment.ExitCode = 2;
        return;
    }

    await SqliteBackup.BackupAsync(options.SqlitePath, args[1], CancellationToken.None);
    Console.WriteLine($"backup written to {Path.GetFullPath(args[1])}");
    return;
}

var builder = WebApplication.CreateSlimBuilder(new WebApplicationOptions
{
    Args = args,
    ContentRootPath = AppContext.BaseDirectory,
});

builder.SetupLogging();
builder.Services.ConfigureHttpJsonOptions(json =>
{
    json.SerializerOptions.TypeInfoResolverChain.Insert(0, AdminJsonContext.Default);
});

StartupSecurity.Validate(options);
builder.WebHost.ConfigureKestrel(kestrel =>
{
    // Artifacts travel base64-encoded inside JSON, so the request body limit is
    // derived from the artifact limit.
    kestrel.Limits.MaxRequestBodySize = checked((options.MaxArtifactBytes * 4 / 3) + (1L << 20));
    ListenEndpoint.Add(kestrel, options.GrpcListen, HttpProtocols.Http2, options.GrpcTls);
    ListenEndpoint.Add(kestrel, options.HttpListen, HttpProtocols.Http1);
});

builder.Services.AddSingleton(options);
builder.RegisterServices(options);

builder.Services.AddGrpc();
builder.Services.AddSingleton<AgentControlService>();
builder.Services.AddSingleton<AgentEnrollmentService>();
builder.Services.AddHostedService<DeploymentReaper>();

var app = builder.Build();

if (options.Local)
{
    app.Logger.LogWarning(
        "WASMGATE_CP_LOCAL is enabled: every process that can reach the loopback listeners is trusted. " +
        "Use this mode only on a single-user host; for a shared host or a fleet use TLS/mTLS.");
}

SqliteBackupWorkspace.CleanStale(
    options.BackupTempDirectory,
    TimeSpan.FromHours(24),
    app.Logger);

// Prepare the SQLite store before serving: create its directory, take the
// single-process lock, enable WAL and apply the embedded schema.
await app.Services.GetRequiredService<SqliteStorageInitializer>().InitializeAsync(CancellationToken.None);

// Create the release-signing key before serving: agents read the matching
// public key when they start, and enrollment can precede the first release.
// Leaving it to the first release would make a fresh control plane unusable
// for every agent that verifies release signatures at startup.
app.Services.GetRequiredService<IReleaseSigner>();

// The admin UI is a static SPA served from wwwroot. It uses the admin API on the
// same origin, so no CORS is involved; the API itself stays protected by the
// bearer token.
app.UseDefaultFiles();
app.UseStaticFiles();

var controlMetrics = app.Services.GetRequiredService<ControlMetrics>();
app.Use(async (context, next) =>
{
    await next(context);
    if (AdminToken.IsProtectedPath(context.Request.Path))
    {
        controlMetrics.RecordHttp(context.Request.Method, context.Response.StatusCode);
    }
});

app.UseAdminHostFilter(options);
app.UseAdminToken(options.AdminToken);

app.MapGrpcService<AgentControlService>();
app.MapGrpcService<AgentEnrollmentService>();
app.MapGrpcService<ArtifactDownloadService>();
app.MapAdminEndpoints();

await app.RunAsync();

/// <summary>Entry point type used by the HTTP integration tests.</summary>
public partial class Program;
