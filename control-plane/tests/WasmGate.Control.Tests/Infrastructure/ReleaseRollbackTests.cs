using WasmGate.Control.Core.Entities.Releases;

namespace WasmGate.Control.Tests.Infrastructure;

/// <summary>Tests for choosing the release a rollback deploys.</summary>
public sealed class ReleaseRollbackTests
{
    private static Release Release(Guid id, ReleaseStatus status = ReleaseStatus.Validated) => new()
    {
        Id = id,
        Environment = "prod",
        ConfigVersionId = Guid.NewGuid(),
        ConfigSha256 = "sha",
        Status = status,
        CreatedBy = "test",
        CreatedAt = DateTimeOffset.UtcNow,
    };

    [Fact]
    public void ReturnsTheNextOlderRelease()
    {
        var newest = Release(Guid.NewGuid());
        var middle = Release(Guid.NewGuid());
        var oldest = Release(Guid.NewGuid());

        var previous = ReleaseRollback.FindPrevious([newest, middle, oldest], middle.Id);

        Assert.Same(oldest, previous);
    }

    [Fact]
    public void SkipsRevokedReleases()
    {
        var current = Release(Guid.NewGuid());
        var revoked = Release(Guid.NewGuid(), ReleaseStatus.Revoked);
        var deployable = Release(Guid.NewGuid());

        var previous = ReleaseRollback.FindPrevious([current, revoked, deployable], current.Id);

        Assert.Same(deployable, previous);
    }

    [Fact]
    public void ReturnsNullForTheOldestRelease()
    {
        var newest = Release(Guid.NewGuid());
        var oldest = Release(Guid.NewGuid());

        Assert.Null(ReleaseRollback.FindPrevious([newest, oldest], oldest.Id));
    }

    [Fact]
    public void ReturnsNullWhenNoOlderReleaseIsDeployable()
    {
        var current = Release(Guid.NewGuid());
        var revoked = Release(Guid.NewGuid(), ReleaseStatus.Revoked);

        Assert.Null(ReleaseRollback.FindPrevious([current, revoked], current.Id));
    }

    [Fact]
    public void ReturnsNullForUnknownRelease()
    {
        Assert.Null(ReleaseRollback.FindPrevious([Release(Guid.NewGuid())], Guid.NewGuid()));
    }
}
