using System.Collections.Concurrent;
using WasmGate.Control.Core.Abstractions.Agents;
using WasmGate.Control.Core.Entities.Delivery;
using WasmGate.Control.Infrastructure.Agents;

namespace WasmGate.Control.Tests.Infrastructure;

/// <summary>Tests the production registry and per-agent lifecycle lock.</summary>
public sealed class AgentLifecycleLockTests
{
    private static readonly TimeSpan Timeout = TimeSpan.FromSeconds(5);

    [Fact]
    public void UnregisterDoesNotRemoveANewerSession()
    {
        var registry = new InMemoryAgentSessionRegistry();
        var agentId = Guid.NewGuid();
        var first = new TestSession(agentId);
        var second = new TestSession(agentId);

        Assert.Null(registry.Register(agentId, first));
        Assert.Same(first, registry.Register(agentId, second));
        registry.Unregister(agentId, first);

        Assert.True(registry.TryGet(agentId, out var current));
        Assert.Same(second, current);
    }

    [Fact]
    public async Task SameAgentOperationsAreSerialized()
    {
        var lifecycle = new AgentOperationLock();
        var agentId = Guid.NewGuid();
        var entered = 0;
        var maximum = 0;
        var firstEntered = new TaskCompletionSource(
            TaskCreationOptions.RunContinuationsAsynchronously);
        var firstMayLeave = new TaskCompletionSource(
            TaskCreationOptions.RunContinuationsAsynchronously);
        var secondStarted = new TaskCompletionSource(
            TaskCreationOptions.RunContinuationsAsynchronously);
        var secondEntered = new TaskCompletionSource(
            TaskCreationOptions.RunContinuationsAsynchronously);

        async Task RunFirstAsync()
        {
            await using var lease = await lifecycle.AcquireAsync(
                agentId,
                CancellationToken.None);
            InterlockedMax(ref maximum, Interlocked.Increment(ref entered));
            firstEntered.TrySetResult();
            await firstMayLeave.Task;
            Interlocked.Decrement(ref entered);
        }

        async Task RunSecondAsync()
        {
            secondStarted.TrySetResult();
            await using var lease = await lifecycle.AcquireAsync(
                agentId,
                CancellationToken.None);
            InterlockedMax(ref maximum, Interlocked.Increment(ref entered));
            secondEntered.TrySetResult();
            Interlocked.Decrement(ref entered);
        }

        var firstTask = RunFirstAsync();
        await firstEntered.Task.WaitAsync(Timeout);
        var secondTask = RunSecondAsync();
        await secondStarted.Task.WaitAsync(Timeout);
        Assert.False(secondEntered.Task.IsCompleted);

        firstMayLeave.TrySetResult();
        await Task.WhenAll(firstTask, secondTask);
        Assert.Equal(1, maximum);
        Assert.True(secondEntered.Task.IsCompleted);
    }

    [Fact]
    public async Task EntryIsRemovedAfterTheLastLeaseIsReleased()
    {
        var lifecycle = new AgentOperationLock();
        var agentId = Guid.NewGuid();

        var lease = await lifecycle.AcquireAsync(agentId, CancellationToken.None);
        Assert.Equal(1, lifecycle.EntryCount);

        await lease.DisposeAsync();
        Assert.Equal(0, lifecycle.EntryCount);
    }

    [Fact]
    public async Task EntryStaysWhileAWaiterIsQueued()
    {
        var lifecycle = new AgentOperationLock();
        var agentId = Guid.NewGuid();
        var first = await lifecycle.AcquireAsync(agentId, CancellationToken.None);
        var secondTask = lifecycle.AcquireAsync(agentId, CancellationToken.None);

        Assert.Equal(1, lifecycle.EntryCount);
        await first.DisposeAsync();

        var second = await secondTask.WaitAsync(Timeout);
        Assert.Equal(1, lifecycle.EntryCount);
        await second.DisposeAsync();
        Assert.Equal(0, lifecycle.EntryCount);
    }

    [Fact]
    public async Task CanceledWaiterDoesNotLeaveAnEntry()
    {
        var lifecycle = new AgentOperationLock();
        var agentId = Guid.NewGuid();
        var first = await lifecycle.AcquireAsync(agentId, CancellationToken.None);
        using var cancellation = new CancellationTokenSource();
        var secondTask = lifecycle.AcquireAsync(agentId, cancellation.Token);

        await cancellation.CancelAsync();
        await Assert.ThrowsAnyAsync<OperationCanceledException>(() => secondTask);
        Assert.Equal(1, lifecycle.EntryCount);

        await first.DisposeAsync();
        Assert.Equal(0, lifecycle.EntryCount);
    }

    [Fact]
    public async Task CanceledSoleWaiterRemovesItsEntry()
    {
        var lifecycle = new AgentOperationLock();
        var agentId = Guid.NewGuid();
        using var cancellation = new CancellationTokenSource();
        await cancellation.CancelAsync();

        await Assert.ThrowsAnyAsync<OperationCanceledException>(
            () => lifecycle.AcquireAsync(agentId, cancellation.Token));
        Assert.Equal(0, lifecycle.EntryCount);
    }

    [Fact]
    public async Task DoubleDisposeReleasesTheLockOnce()
    {
        var lifecycle = new AgentOperationLock();
        var agentId = Guid.NewGuid();
        var lease = await lifecycle.AcquireAsync(agentId, CancellationToken.None);

        await lease.DisposeAsync();
        await lease.DisposeAsync();

        Assert.Equal(0, lifecycle.EntryCount);
        await using var next = await lifecycle.AcquireAsync(agentId, CancellationToken.None);
        Assert.Equal(1, lifecycle.EntryCount);
    }

    [Fact]
    public async Task ExceptionInsideLeaseReleasesTheLock()
    {
        var lifecycle = new AgentOperationLock();
        var agentId = Guid.NewGuid();

        await Assert.ThrowsAsync<InvalidOperationException>(async () =>
        {
            await using var lease = await lifecycle.AcquireAsync(
                agentId,
                CancellationToken.None);
            throw new InvalidOperationException("operation failed");
        });

        Assert.Equal(0, lifecycle.EntryCount);
        await using var next = await lifecycle.AcquireAsync(agentId, CancellationToken.None);
        Assert.Equal(1, lifecycle.EntryCount);
    }

    [Fact]
    public async Task DifferentAgentsCanProceedIndependently()
    {
        var lifecycle = new AgentOperationLock();
        var entered = new ConcurrentBag<Guid>();
        var bothEntered = new TaskCompletionSource(
            TaskCreationOptions.RunContinuationsAsynchronously);
        var enteredCount = 0;

        async Task RunAsync(Guid agentId)
        {
            await using var lease = await lifecycle.AcquireAsync(
                agentId,
                CancellationToken.None);
            entered.Add(agentId);
            if (Interlocked.Increment(ref enteredCount) == 2)
            {
                bothEntered.TrySetResult();
            }

            await bothEntered.Task.WaitAsync(Timeout);
        }

        await Task.WhenAll(RunAsync(Guid.NewGuid()), RunAsync(Guid.NewGuid()));
        Assert.Equal(2, entered.Count);
    }

    private static void InterlockedMax(ref int location, int value)
    {
        while (true)
        {
            var current = Volatile.Read(ref location);
            if (value <= current || Interlocked.CompareExchange(ref location, value, current) == current)
            {
                return;
            }
        }
    }

    private sealed class TestSession(Guid agentId) : IAgentSession
    {
        public Guid AgentId { get; } = agentId;

        public ValueTask SendAsync(AgentCommand command, CancellationToken cancellationToken) =>
            ValueTask.CompletedTask;

        public ValueTask CloseAsync(CancellationToken cancellationToken) =>
            ValueTask.CompletedTask;
    }
}
