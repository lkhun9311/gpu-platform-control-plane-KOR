package v1

// QuotaEnforcedLabel marks a namespace whose GPU admission guards are active.
//
// It exists as a Go constant because a controller has to write it, and it is here rather than beside the
// webhook that consumes it because neither side owns it: the webhook's ValidatingWebhookConfiguration
// selects on it in YAML, and the GPUQuotaPolicy controller stamps it in Go. A constant living in one of the
// two would be a constant the other copied.
//
// What it is for is narrower than "GPU namespace". The guards refuse a Pod on the grounds that the
// namespace's GPU budget is held by a Kueue ClusterQueue and nothing would charge a direct request against
// it. That sentence is only true where a GPUQuotaPolicy exists, so the label follows the policy rather than
// being applied cluster-wide -- a namespace with no quota to protect has nothing for the guard to protect it
// from, and refusing there would be the guard exceeding its own justification.
//
// The failure this closes is not hypothetical. The guard was built, attacked, broken, rebuilt on requester
// identity and re-attacked on a live cluster, and its coverage was still a fact about a shell transcript:
// nothing checked into the repository labelled any namespace, and neither deployment overlay even contained
// the webhook. A guard that only exists where somebody remembered to type kubectl label is not a platform
// control, whatever its tests say.
const QuotaEnforcedLabel = "platform.lkhun9311.github.io/gpu-quota-enforced"

// QuotaEnforcedValue is the only value the selector matches, so a namespace carrying anything else is not
// guarded. It is a constant for the same reason the key is: the selector is matchLabels, not Exists.
const QuotaEnforcedValue = "true"
