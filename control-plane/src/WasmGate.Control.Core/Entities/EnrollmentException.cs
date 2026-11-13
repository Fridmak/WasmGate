namespace WasmGate.Control.Core.Entities;

/// <summary>Reason an enrollment or provisioning request was rejected.</summary>
public enum EnrollmentFailure
{
    /// <summary>The token is unknown or invalid.</summary>
    InvalidToken = 0,

    /// <summary>The token has expired.</summary>
    Expired = 1,

    /// <summary>The token reached its maximum number of uses.</summary>
    Exhausted = 2,

    /// <summary>The token is bound to a different agent.</summary>
    AgentMismatch = 3,

    /// <summary>The CSR is malformed or its subject does not match the agent.</summary>
    CsrInvalid = 4,

    /// <summary>An agent with the same name already exists.</summary>
    AlreadyExists = 5,
}

/// <summary>Thrown when enrollment or agent provisioning fails.</summary>
public sealed class EnrollmentException(EnrollmentFailure failure, string message) : Exception(message)
{
    /// <summary>Machine-readable failure kind.</summary>
    public EnrollmentFailure Failure { get; } = failure;
}
