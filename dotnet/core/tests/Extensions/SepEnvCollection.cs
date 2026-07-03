namespace SmooAI.SmoothOperator.Core.Tests.Extensions;

/// <summary>Serializes tests that mutate process-global environment variables (SMOOTH_HOME,
/// SMOOTH_EXTENSIONS_*, and <c>${env:VAR}</c> expansion probes) so they never race each other.</summary>
[CollectionDefinition("SepEnv", DisableParallelization = true)]
public sealed class SepEnvCollection { }
