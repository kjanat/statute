package statute

type (
	workloadBindingKey uint64
	dockerProvider     struct{ nextWorkloadBinding workloadBindingKey }
)

func (p *dockerProvider) nextWorkloadBindingLocked() workloadBindingKey {
	p.nextWorkloadBinding++
	return 1 // want "\\[SLC105\\].*unique incremented provider counter"
}

func bad(p *dockerProvider) {
	alias := &p.nextWorkloadBinding
	*alias = 0 // want "\\[SLC105\\].*immutable mutation binding"
}
