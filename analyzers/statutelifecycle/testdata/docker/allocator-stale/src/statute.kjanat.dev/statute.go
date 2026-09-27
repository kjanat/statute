package statute

type workloadBindingKey uint64
type dockerProvider struct{ nextWorkloadBinding workloadBindingKey }

func (p *dockerProvider) nextWorkloadBindingLocked() workloadBindingKey {
	previous := p.nextWorkloadBinding
	p.nextWorkloadBinding++
	return previous // want "\\[SLC105\\].*unique incremented provider counter"
}
