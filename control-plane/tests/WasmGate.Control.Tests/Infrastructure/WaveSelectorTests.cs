using WasmGate.Control.Core.Entities.Agents;
using WasmGate.Control.Infrastructure.Agents;

namespace WasmGate.Control.Tests.Infrastructure;

/// <summary>Tests for wave selector parsing and label matching.</summary>
public sealed class WaveSelectorTests
{
    private static Agent Agent(params (string Key, string Value)[] labels) => new()
    {
        Name = "edge-1",
        Version = "test",
        Labels = labels.ToDictionary(pair => pair.Key, pair => pair.Value, StringComparer.Ordinal),
    };

    [Theory]
    [InlineData("{\"role\":\"edge\"}")]
    [InlineData("{\"role\":\"edge\",\"region\":\"eu\"}")]
    public void MatchesWhenEveryLabelEquals(string selector)
    {
        var parsed = WaveSelector.Parse(selector);
        var agent = Agent(("role", "edge"), ("region", "eu"));

        Assert.True(parsed.Matches(agent));
    }

    [Fact]
    public void WildcardMatchesEveryone()
    {
        Assert.True(WaveSelector.Parse("*").Matches(Agent()));
    }

    [Theory]
    [InlineData("{\"role\":\"canary\"}")]
    [InlineData("{\"role\":\"EDGE\"}")] // ordinal comparison
    [InlineData("{\"region\":\"us\"}")]
    [InlineData("{\"missing\":\"value\"}")]
    public void DoesNotMatchDifferentOrMissingLabels(string selector)
    {
        var parsed = WaveSelector.Parse(selector);
        var agent = Agent(("role", "edge"), ("region", "eu"));

        Assert.False(parsed.Matches(agent));
    }

    [Theory]
    [InlineData("")]
    [InlineData("   ")]
    [InlineData("not json")]
    [InlineData("[]")]
    [InlineData("{\"role\":1}")]
    [InlineData("{\"role\":null}")]
    public void RejectsInvalidSelectors(string selector)
    {
        Assert.Throws<InvalidOperationException>(() => WaveSelector.Parse(selector));
    }
}
