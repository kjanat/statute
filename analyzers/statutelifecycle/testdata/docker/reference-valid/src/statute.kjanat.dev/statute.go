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
	alias := b
	if alias == nil {
		return ""
	}
	identifier := alias.containerID
	if identifier == "" {
		return alias.container
	}
	return identifier
}

func (w *workload) callRef(key workloadBindingKey, fallback string) string {
	alias := w
	if alias.binding == nil {
		return fallback
	}
	binding := alias.binding
	if key != binding.key {
		return fallback
	}
	reference := binding.ref()
	if reference == "" {
		return fallback
	}
	return reference
}
