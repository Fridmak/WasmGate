using WasmGate.Control.Api;
using WasmGate.Control.Core.Configuration;

namespace WasmGate.Control.Tests.Storage;

/// <summary>
/// Verifies the storage-provider selection matrix without touching process
/// environment variables.
/// </summary>
public sealed class StorageSelectionTests
{
    [Fact]
    public void DefaultsToPostgresWhenNothingIsConfigured()
    {
        Assert.Equal(StorageProvider.Postgres, StartUp.ParseStorage(null, null, null));
    }

    [Fact]
    public void PostgresConnectionStringSelectsPostgres()
    {
        Assert.Equal(
            StorageProvider.Postgres,
            StartUp.ParseStorage(null, "Host=localhost", null));
    }

    [Fact]
    public void SqlitePathAloneSelectsSqlite()
    {
        Assert.Equal(
            StorageProvider.Sqlite,
            StartUp.ParseStorage(null, null, "data/control.db"));
    }

    [Theory]
    [InlineData("sqlite")]
    [InlineData("SQLITE")]
    [InlineData(" sqlite ")]
    public void ExplicitSqliteSelectsSqlite(string raw)
    {
        Assert.Equal(StorageProvider.Sqlite, StartUp.ParseStorage(raw, null, null));
    }

    [Theory]
    [InlineData("postgres")]
    [InlineData("POSTGRES")]
    public void ExplicitPostgresSelectsPostgres(string raw)
    {
        Assert.Equal(StorageProvider.Postgres, StartUp.ParseStorage(raw, null, null));
    }

    [Fact]
    public void SqliteWithPostgresConnectionStringIsRejected()
    {
        var exception = Assert.Throws<InvalidOperationException>(
            () => StartUp.ParseStorage("sqlite", "Host=localhost", null));
        Assert.Contains("WASMGATE_CP_POSTGRES", exception.Message);
    }

    [Fact]
    public void UnknownProviderIsRejected()
    {
        var exception = Assert.Throws<InvalidOperationException>(
            () => StartUp.ParseStorage("mysql", null, null));
        Assert.Contains("postgres, sqlite", exception.Message);
    }
}
