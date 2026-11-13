using Dapper;

// Enable Dapper.AOT globally for this assembly. Generated interceptors replace
// runtime reflection/IL-emit, so the same repository code works under JIT and
// Native AOT.
[module: DapperAot]
