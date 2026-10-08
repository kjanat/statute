package statute

type (
	workloadBindingKey uint64
	workloadBinding    struct {
		key         workloadBindingKey
		container   string
		containerID string
	}
)
type workload struct{ binding *workloadBinding }

func (b *workloadBinding) ref() string {
	if b == nil {
		return ""
	}
	return b.container // want "\\[SLC105\\].*prefer its immutable container ID"
}

func (w *workload) callRef(key workloadBindingKey, fallback string) string {
	if w.binding == nil {
		return fallback
	}
	return w.binding.ref() // want "\\[SLC105\\].*preserve the operation binding"
}
