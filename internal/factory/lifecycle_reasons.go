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

// LifecycleReasonHarnessMissing identifies a pause after a launch on a worker
// image that has no executable for the selected harness. A Run keeps its
// frozen image, so neither an automatic nor an explicit resume can succeed;
// an operator upgrades the worker and cancels the Run.
const LifecycleReasonHarnessMissing = "harness missing from worker image"

// LifecycleReasonCheckInfrastructureUnavailable identifies a check pause whose
// gate suite or repair launch failed for infrastructure reasons. An explicit
// resume retries the checks at the same checkpoint without spending a repair
// attempt.
const LifecycleReasonCheckInfrastructureUnavailable = "check infrastructure unavailable"
