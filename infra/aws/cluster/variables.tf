variable "region" {
  description = "AWS region."
  type        = string
  # ap-northeast-2, because that is where the GPU quota lives.
  #
  # This defaulted to us-east-1 while the account's "Running On-Demand G and VT instances" quota was raised
  # to 52 vCPU in Seoul on 2026-08-26. The mismatch would not have failed the apply: every GPU node group
  # runs at desired_size 0, so no G instance is launched until a session scales one up -- and the quota error
  # would have arrived there, on a cluster that had looked fine for hours.
  #
  # Changing it means the state bucket, the cluster and the registry all live beside the quota. A region set
  # in one place and not the others is the same failure with an extra step.
  default = "ap-northeast-2"
}

variable "cluster_name" {
  description = "EKS cluster name."
  type        = string
  default     = "gpu-platform"
}

variable "cluster_version" {
  description = "Pinned EKS Kubernetes version. Clusters are recreated, never upgraded."
  type        = string

  # 1.35, because 1.31 left STANDARD support on 2025-11-26 and this cluster would have been billed at the
  # extended-support rate -- which AWS publishes at $0.60/cluster-hour against the $0.10 this repository's
  # cost model quotes. Six times the control-plane cost, on a line item the cost table called verified.
  #
  # The version status is not a matter of opinion and does not need a documentation lookup:
  #   aws eks describe-cluster-versions --region ap-northeast-2
  # returned EXTENDED_SUPPORT for 1.31, 1.32 and 1.33 on 2026-08-27, and STANDARD_SUPPORT for 1.34
  # (standard until 2026-12-02), 1.35 (2027-03-27) and 1.36 (2027-08-02, the current default).
  #
  # 1.35 rather than the 1.36 default: an ephemeral cluster gains nothing from the newest minor, and 1.35's
  # standard support outlasts any plausible session here by months. 1.34 was rejected because its standard
  # support ends in December, which is close enough that a later session would silently land on the extended
  # rate again -- the exact failure being fixed.
  #
  # Changing this REQUIRES re-resolving the add-on versions in eks.tf against
  # `aws eks describe-addon-versions --kubernetes-version <v>`. A pinned add-on built for another minor is
  # not a warning; it is a CreateAddon failure at apply.
  default = "1.35"
}

variable "vpc_cidr" {
  description = "VPC CIDR block."
  type        = string
  default     = "10.0.0.0/16"
}

variable "node_instance_type" {
  description = "Instance type for the CPU managed node group."
  type        = string
  default     = "t3.large"
}

variable "gpu_shared_node_instance_type" {
  description = "Instance type for the sharing node group M5-c runs its matrix on."
  type        = string
  # ONE A10G, and 24 GB is the requirement rather than a preference.
  #
  # The matrix puts two Qwen2.5-3B engines on one card. Time-slicing does not partition memory, so each gets
  # half: on a T4 that leaves 10 MiB of KV per engine -- 284 tokens against a 7,695-token contender prompt --
  # and internal/bench.SharingPlan refuses it. An A10G leaves 3.6 GiB each.
  #
  # Four vCPU, the same as g4dn.xlarge, so the 8 vCPU granted for ap-northeast-2 on 2026-08-24 covers it:
  # M5-c is not blocked on the support case either.
  default = "g5.xlarge"
}

variable "gpu_single_node_instance_type" {
  description = "Instance type for the one-card GPU node group that M5-b runs its four arms on."
  type        = string
  # ONE A10G, and it is the same card the sharing matrix needs -- which is the point.
  #
  # This was g4dn.xlarge (a T4) until the M5-c sizing forced an A10G: two Qwen2.5-3B engines leave 10 MiB of
  # KV each on a T4, so the matrix cannot run there at all. Leaving M5-b on the T4 would have put the
  # flagship and the matrix on different silicon, and the exclusive arm of the matrix IS the flagship's
  # configuration -- one engine with the card to itself. On one card class they are the same measurement and
  # M5-c does not have to pay for that arm twice.
  #
  # Four vCPU, the same as g4dn.xlarge, so the 8 vCPU granted for ap-northeast-2 on 2026-08-24 still covers
  # it, and the A10G's higher throughput means each arm takes less wall-clock despite the higher hourly rate.
  #
  # config/vllm/deployment.yaml keeps --dtype=half, which on sm_86 is a CHOICE rather than the startup
  # condition it was on sm_75. It stays because the matrix and the flagship must not differ in dtype and
  # nothing has been measured that would justify moving.
  default = "g5.xlarge"
}

variable "gpu_node_instance_type" {
  description = "Instance type for the GPU managed node group, which runs at desired_size 0 until a session."
  type        = string
  # FOUR T4s, and the count is the requirement rather than a preference.
  #
  # The protocol needs TWO devices on ONE node at the same time: the trace runs a1 inside the tenant's quota
  # and a2-borrow beyond it concurrently, and the lab pins both to a single worker it holds exclusively. Every
  # recorded run carries requiredGPU 2 against allocatable 2, and qualifyWorker refuses a node that cannot
  # meet it.
  #
  # This was g5.xlarge, which has ONE A10G. A review caught it before any money was spent: the node group
  # would have provisioned, the first run would have been refused at qualification for a node too small to
  # measure on, and the bill would already have started. Nothing in terraform can check a Kubernetes-side
  # requirement, so it is written here where the number is chosen.
  #
  # g4dn.12xlarge is the cheapest instance carrying more than one GPU that DCGM supports properly -- Turing,
  # four T4s.
  #
  # This comment used to say "the two spare cards cost nothing the experiment reads". That was asserted
  # without measuring and it was false. The study's headline is the owner's wait, and the records show what
  # produces it: Kueue admits the owner within 0.1 s of the preemption decision, and the owner's Pod then
  # becomes Ready one to two seconds after the VICTIM'S terminal phase -- it is waiting for a card. Two idle
  # cards would absorb it at admission in both arms and the 29-second difference would collapse below the
  # floor, with every other figure in the record looking exactly as it does now.
  #
  # The spare cards are therefore excluded rather than tolerated. The first attempt at that was a
  # NVIDIA_VISIBLE_DEVICES setting on the device plugin, which two reviews then established almost certainly
  # does not restrict what the plugin advertises -- see the comment beside it in
  # config/nvidia-device-plugin/daemonset.yaml. The exclusion the run actually relies on is its own: the
  # qualification refuses a node advertising more devices than the protocol needs, and the run occupies the
  # surplus itself so that what it measures is a node with exactly the scarcity the contrast depends on.
  #
  # Anything with two or more well-supported devices works, because the surplus is taken away by the harness
  # rather than trusted to be harmless or trusted to a manifest nobody can check without hardware.
  default = "g4dn.12xlarge"
}

variable "tags" {
  description = "Owner and TTL tags applied to every resource."
  type        = map(string)
  default = {
    project = "gpu-platform-control-plane"
    owner   = "lkhun9311"
    ttl     = "ephemeral"
  }
}

variable "ci_apply_role_name" {
  description = "Name of the CI apply role (created by infra/aws/bootstrap) granted cluster admin via an access entry. Empty disables the entry."
  type        = string

  # A NAME that is looked up, not an ARN that must be passed in.
  #
  # This was `ci_apply_role_arn`, defaulting to "", and iam.tf counts the access entry on it being non-empty.
  # Nothing ever passed it -- not infra.yml, not destroy.yml, not any local invocation -- so `count` was 0 on
  # every plan this repository has ever produced and the CI role has never had an access entry. The nightly
  # teardown's kubectl phase could not have authenticated even from inside the VPC.
  #
  # The bug is not that someone forgot the flag. It is that the design required a value to be carried by hand
  # from one Terraform state to another across three call sites, and a value carried by hand is a value that
  # gets dropped. Looking the role up by its known name makes the wiring structural: there is nothing left to
  # forget, and if bootstrap has not run the data source fails loudly instead of silently producing count 0.
  #
  # Empty still disables it, for a cluster deliberately built without CI access.
  default = "gpu-platform-ci-apply"
}

variable "api_public_access_cidrs" {
  description = "CIDRs allowed to reach the EKS public API endpoint. Empty disables the public endpoint entirely."
  type        = list(string)

  # Empty by default, which means no public endpoint at all.
  #
  # The module's own default is ["0.0.0.0/0"], and because the repository never set this argument that is
  # what the cluster would have been built with -- an API server every scanner on the internet can reach.
  # IAM and RBAC still stand in front of it, so this is not an open cluster; it is an open FRONT DOOR, and
  # the difference matters exactly when a credential leaks.
  #
  # A session that wants kubectl from a laptop passes its own address:
  #   terraform apply -var='api_public_access_cidrs=["203.0.113.7/32"]'
  # That is a home or office egress address, so the allowance covers everything behind that router, and it
  # changes when the ISP reassigns it. The alternative with no such caveat is to leave this empty and reach
  # the API through SSM.
  default = []

  # Two checks, and what they are is worth being precise about: these are ACCIDENT GUARDS, not a security
  # boundary. The security boundary is IAM plus RBAC. A CIDR list only decides who can reach the endpoint to
  # be rejected by them, and a review pointed out that calling the first check an "allow-list validator"
  # overstated it -- rejecting the literal 0.0.0.0/0 leaves 0.0.0.0/1 plus 128.0.0.0/1, or forty ranges
  # covering nearly all of IPv4, passing untouched.
  #
  # The second check is what closes that. Together they stop the two ways this field goes wrong by accident:
  # inheriting the module's world-open default, and pasting something far broader than intended.
  validation {
    # 0.0.0.0/0 is indistinguishable from not setting the argument at all, which is the state this variable
    # exists to end. Someone who genuinely wants a world-reachable endpoint can delete these checks and
    # explain why in the commit; what must not happen is arriving there by leaving a field blank.
    condition     = !contains(var.api_public_access_cidrs, "0.0.0.0/0")
    error_message = "0.0.0.0/0 is not an allow-list. Name the addresses, or leave the list empty and reach the API through SSM."
  }

  validation {
    # /24 admits a home or small-office egress range and refuses anything that reads as a region-sized block.
    # It is a breadth cap rather than a judgement about any particular network: a single operator's address
    # is a /32, and a list of /8s is a mistake regardless of whose /8 it is.
    condition = alltrue([
      for c in var.api_public_access_cidrs :
      can(cidrhost(c, 0)) && tonumber(split("/", c)[1]) >= 24
    ])
    error_message = "Every entry must be a valid IPv4 CIDR of /24 or narrower. A broader block is either a mistake or a decision that belongs in a commit message rather than in this list."
  }
}

variable "gpu_max_nodes" {
  description = "max_size for the queuelab GPU node group. 2 enables the two-node axis and requires 96 vCPU of G/VT quota."
  type        = number

  # 1, and this default is a statement about the ACCOUNT rather than about the experiment.
  #
  # queuelab's preregistration includes a two-node axis, and the group's cap was deliberately raised to 2 for
  # it -- the comment in eks.tf argues at length that a cap of 1 saved nothing and made a promised comparison
  # impossible. That argument still holds. What it did not account for is that g4dn.12xlarge is 48 vCPU, so
  # two of them ask for 96 against the 52 the account was granted on 2026-08-26.
  #
  # The failure would not have been a refusal. The group would scale to one node, stall, and the second node
  # would simply never arrive -- during a session, while the first one bills $4.812/hour.
  #
  # So the capability is gated on the quota rather than removed. Raise this to 2 after the increase to at
  # least 96 vCPU is granted; terraform_data.gpu_quota refuses the plan until then, which is what makes this
  # a gate and not a note.
  default = 1

  validation {
    condition     = var.gpu_max_nodes >= 1 && var.gpu_max_nodes <= 2
    error_message = "The protocol uses one or two workers. Anything else is not a configuration this study describes."
  }
}
