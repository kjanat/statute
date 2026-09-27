package statute

type workloadBindingKey uint64
type dockerProvider struct{ nextWorkloadBinding workloadBindingKey }

func (p *dockerProvider) nextWorkloadBindingLocked() workloadBindingKey {
	alias := p
	alias.nextWorkloadBinding++
	result := (alias.nextWorkloadBinding)
	return result
}
