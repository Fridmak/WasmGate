namespace WasmGate.Control.Infrastructure.Storage;

/// <summary>
/// Creates backups in a private workspace and owns their lifetime.
/// </summary>
/// <remarks>
/// A download owns the workspace and the coordinator lease. Both are released
/// when the response stream is disposed, whether the client reads the whole
/// snapshot, disconnects, or the request is cancelled before streaming starts.
/// </remarks>
public sealed class SqliteBackupDownload : IDisposable
{
    private readonly string _filePath;
    private readonly SqliteBackupWorkspace _workspace;
    private readonly IDisposable? _lease;

    private int _disposed;

    internal SqliteBackupDownload(
        string filePath,
        SqliteBackupWorkspace workspace,
        IDisposable? lease)
    {
        _filePath = filePath;
        _workspace = workspace;
        _lease = lease;
    }

    /// <summary>Opens the snapshot for reading.</summary>
    /// <returns>
    /// A read-only stream that removes the snapshot and releases the backup slot
    /// when it is disposed.
    /// </returns>
    /// <exception cref="ObjectDisposedException">The download was already released.</exception>
    public Stream OpenRead()
    {
        ObjectDisposedException.ThrowIf(
            Volatile.Read(ref _disposed) != 0,
            this);

        var file = new FileStream(
            _filePath,
            FileMode.Open,
            FileAccess.Read,
            FileShare.Read,
            bufferSize: 64 * 1024,
            FileOptions.Asynchronous);

        return new DownloadStream(file, this);
    }

    /// <summary>Removes the snapshot and releases the backup slot.</summary>
    public void Dispose()
    {
        if (Interlocked.Exchange(ref _disposed, 1) != 0)
        {
            return;
        }

        _workspace.Dispose();
        _lease?.Dispose();
    }

    /// <summary>Closes the file before removing the workspace.</summary>
    private sealed class DownloadStream(Stream inner, SqliteBackupDownload owner) : Stream
    {
        public override bool CanRead => inner.CanRead;

        public override bool CanSeek => inner.CanSeek;

        public override bool CanWrite => false;

        public override long Length => inner.Length;

        public override long Position
        {
            get => inner.Position;
            set => inner.Position = value;
        }

        public override void Flush() => inner.Flush();

        public override Task FlushAsync(CancellationToken cancellationToken) =>
            inner.FlushAsync(cancellationToken);

        public override int Read(byte[] buffer, int offset, int count) =>
            inner.Read(buffer, offset, count);

        public override int Read(Span<byte> buffer) => inner.Read(buffer);

        public override Task<int> ReadAsync(
            byte[] buffer,
            int offset,
            int count,
            CancellationToken cancellationToken) =>
            inner.ReadAsync(buffer, offset, count, cancellationToken);

        public override ValueTask<int> ReadAsync(
            Memory<byte> buffer,
            CancellationToken cancellationToken = default) =>
            inner.ReadAsync(buffer, cancellationToken);

        public override long Seek(long offset, SeekOrigin origin) =>
            inner.Seek(offset, origin);

        public override void SetLength(long value) =>
            throw new NotSupportedException();

        public override void Write(byte[] buffer, int offset, int count) =>
            throw new NotSupportedException();

        public override void Write(ReadOnlySpan<byte> buffer) =>
            throw new NotSupportedException();

        public override Task WriteAsync(
            byte[] buffer,
            int offset,
            int count,
            CancellationToken cancellationToken) =>
            throw new NotSupportedException();

        public override ValueTask WriteAsync(
            ReadOnlyMemory<byte> buffer,
            CancellationToken cancellationToken = default) =>
            throw new NotSupportedException();

        protected override void Dispose(bool disposing)
        {
            if (disposing)
            {
                try
                {
                    inner.Dispose();
                }
                finally
                {
                    owner.Dispose();
                }
            }

            base.Dispose(disposing);
        }

        public override async ValueTask DisposeAsync()
        {
            try
            {
                await inner.DisposeAsync();
            }
            finally
            {
                owner.Dispose();
            }

            GC.SuppressFinalize(this);
        }
    }
}
