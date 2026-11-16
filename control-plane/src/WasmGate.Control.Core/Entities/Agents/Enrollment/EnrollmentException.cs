namespace WasmGate.Control.Core.Entities.Agents.Enrollment;

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

    /// <summary>The CSR is malformed, is not RSA, or has an unsupported key.</summary>
    CsrInvalid = 4,

    /// <summary>An agent with the same name already exists.</summary>
    AlreadyExists = 5,

    /// <summary>The request contains an invalid identifier, name, lifetime or use count.</summary>
    InvalidRequest = 6,
}

/// <summary>Thrown when enrollment or agent provisioning is rejected.</summary>
/// <remarks>
/// The gRPC layer maps <see cref="Failure"/> to a status code, so callers can
/// distinguish a bad token from a bad name without parsing the message.
/// </remarks>
public sealed class EnrollmentException(EnrollmentFailure failure, string message) : Exception(message)
{
    /// <summary>Machine-readable failure kind.</summary>
    public EnrollmentFailure Failure { get; } = failure;
}
