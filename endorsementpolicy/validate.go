package endorsementpolicy

import "errors"

func (c *PolicyConfig) Validate() error {
	if c == nil {
		return errors.New("nil endorsement policy config")
	}
	if c.BlockEndorsementTimeout <= 0 {
		return errors.New("block endorsement timeout must be greater than 0")
	}
	if c.MaxRebuildRounds < 0 {
		return errors.New("max rebuild rounds must be non-negative")
	}
	return nil
}

func (p *EndorsementPolicy) Validate() error {
	if p == nil {
		return errors.New("nil endorsement policy")
	}
	if p.ID == "" {
		return errors.New("empty endorsement policy id")
	}
	if len(p.Endorsers.Members) == 0 {
		return errors.New("empty endorsers")
	}
	if p.Threshold == 0 {
		return errors.New("threshold must be greater than 0")
	}
	if int(p.Threshold) > len(p.Endorsers.Members) {
		return errors.New("threshold exceeds endorser count")
	}
	return nil
}

func (r *PolicyResolution) Validate() error {
	if r == nil {
		return errors.New("nil policy resolution")
	}
	if r.Policy == nil {
		return errors.New("nil resolved policy")
	}
	return r.Policy.Validate()
}