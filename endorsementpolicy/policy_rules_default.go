package endorsementpolicy

import "github.com/ethereum/go-ethereum/common"

func BuildDefaultExperimentRules(
	failAddr common.Address,
	strictPolicy *EndorsementPolicy,
	defaultPolicy *EndorsementPolicy,
) []PolicyRule {
	_ = defaultPolicy // 先保留这个参数，方便以后扩展更多默认规则

	return []PolicyRule{
		{
			ID:          "strict-to-fail-address",
			Priority:    100,
			Description: "transactions sent to the fail address require stricter endorsement",
			Enabled:     true,
			Match: RuleMatch{
				ToEquals: []common.Address{
					failAddr,
				},
			},
			Policy: strictPolicy,
		},
		{
			ID:          "strict-contract-create",
			Priority:    90,
			Description: "contract creation uses strict endorsement policy",
			Enabled:     true,
			Match: RuleMatch{
				IsContractCreate: boolPtr(true),
			},
			Policy: strictPolicy,
		},
	}
}

func boolPtr(v bool) *bool {
	return &v
}