package endorsementpolicy

import "github.com/ethereum/go-ethereum/common"

// 创建了两个背书策略规则，如果 to 地址为 0x1111111111111111111111111111111111111111 或 是合约创建交易，则使用 strict 策略
func BuildDefaultExperimentRules(
	strictPolicy *EndorsementPolicy,
	defaultPolicy *EndorsementPolicy,
) []PolicyRule {
	return []PolicyRule{
		{
			ID:          "strict-to-fail-address",
			Priority:    100,
			Description: "transactions sent to the fail address require stricter endorsement",
			Enabled:     true,
			Match: RuleMatch{
				ToEquals: []common.Address{
					common.HexToAddress("0x1111111111111111111111111111111111111111"),
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