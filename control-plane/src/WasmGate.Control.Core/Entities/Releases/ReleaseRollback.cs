namespace WasmGate.Control.Core.Entities.Releases;

/// <summary>
/// Chooses the release a rollback deploys.
/// </summary>
/// <remarks>
/// Rollback means "deploy the previous release of the same environment". The
/// previous release is the newest older release that is still deployable, so a
/// revoked release in between does not make rollback impossible. The caller must
/// pass releases in the repository order (newest first).
/// </remarks>
public static class ReleaseRollback
{
    /// <summary>Finds the previous deployable release.</summary>
    /// <param name="newestFirst">Releases of one environment, newest first.</param>
    /// <param name="releaseId">Release currently deployed by the deployment.</param>
    /// <returns>The previous non-revoked release, or null when there is none.</returns>
    public static Release? FindPrevious(IReadOnlyList<Release> newestFirst, Guid releaseId)
    {
        ArgumentNullException.ThrowIfNull(newestFirst);

        var currentIndex = -1;
        for (var index = 0; index < newestFirst.Count; index++)
        {
            if (newestFirst[index].Id == releaseId)
            {
                currentIndex = index;
                break;
            }
        }

        if (currentIndex < 0)
        {
            return null;
        }

        for (var index = currentIndex + 1; index < newestFirst.Count; index++)
        {
            if (newestFirst[index].Status != ReleaseStatus.Revoked)
            {
                return newestFirst[index];
            }
        }

        return null;
    }
}
