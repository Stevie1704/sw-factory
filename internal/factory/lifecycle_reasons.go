package factory

// LifecycleReasonHarnessCapacityUnavailable identifies a temporary harness
// capacity pause that an explicit resume may retry.
const LifecycleReasonHarnessCapacityUnavailable = "harness capacity unavailable"

// LifecycleReasonHarnessAuthenticationExpired identifies a credential pause
// that follows an expired harness authentication failure.
const LifecycleReasonHarnessAuthenticationExpired = "harness authentication expired"

// LifecycleReasonAutomaticHarnessRecoveryExhausted identifies the boundary
// after the bounded automatic native-session recovery attempts are consumed.
const LifecycleReasonAutomaticHarnessRecoveryExhausted = "automatic harness recovery exhausted"

// LifecycleReasonHarnessOutOfMemory identifies a pause after the worker memory
// limit killed a detached harness process. An automatic resume would meet the
// same limit, so an operator raises the limit or accepts the risk and resumes.
const LifecycleReasonHarnessOutOfMemory = "harness out of memory"

// LifecycleReasonCheckInfrastructureUnavailable identifies a check pause whose
// gate suite or repair launch failed for infrastructure reasons. An explicit
// resume retries the checks at the same checkpoint without spending a repair
// attempt.
const LifecycleReasonCheckInfrastructureUnavailable = "check infrastructure unavailable"
