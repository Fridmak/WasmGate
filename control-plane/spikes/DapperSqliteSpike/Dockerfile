# syntax=docker/dockerfile:1
# Native AOT publish + clean-runtime smoke test for the SQLite spike.
FROM mcr.microsoft.com/dotnet/sdk:10.0 AS build
RUN apt-get update \
    && apt-get install -y --no-install-recommends clang zlib1g-dev \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /src
COPY DapperSqliteSpike.csproj Program.cs ./
RUN dotnet publish -c Release -r linux-x64 -p:PublishAot=true \
    -p:StripSymbols=true -p:DebugType=None -o /out

# Run on a distroless base: no shell, no .NET runtime, no SDK.
FROM gcr.io/distroless/cc-debian12
WORKDIR /app
COPY --from=build /out/DapperSqliteSpike /out/libe_sqlite3.so /app/
ENTRYPOINT ["/app/DapperSqliteSpike"]
