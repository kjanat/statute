package migrate

import (
	"fmt"

	statute "statute.kjanat.dev"
)

func servicePassiveHealth(o *Object, pool *statute.Pool) []string {
	const base = "loadBalancer.passiveHealthCheck"
	if o.value(base) == nil && !hasFieldPrefix(o, base+".") {
		return nil
	}
	window := "10s"
	if o.value(base+".failureWindow") != nil {
		window = o.text(base + ".failureWindow")
	}
	if !positiveDuration(window) {
		o.mark(base, Manual, "passive health requires a positive literal failure window")
		return nil
	}
	maximum := 1
	if value := o.value(base + ".maxFailedAttempts"); value != nil {
		var err error
		maximum, err = integer(value)
		if err != nil || maximum <= 0 {
			o.mark(base, Manual, "passive health failure threshold must be a positive integer")
			return nil
		}
	}
	pool.PassiveHealthCheck = statute.PassiveHealthCheck{FailureWindow: window, MaxFailures: maximum}
	if value, ok := o.value(base).(map[string]any); ok && len(value) == 0 {
		o.mark(base, Translated, "native passive-health candidate with source defaults")
	}
	o.mark(base+".failureWindow", Translated, "pool-owned passive-health window")
	o.mark(base+".maxFailedAttempts", Translated, "pool-owned passive-health threshold")
	return []string{fmt.Sprintf("PassiveHealthCheck:statute.PassiveHealthCheck{FailureWindow:%q,MaxFailures:%d}", window, maximum)}
}

func checkPassiveHealthBehavior(o *Object) {
	const base = "loadBalancer.passiveHealthCheck"
	if o.value(base) == nil && !hasFieldPrefix(o, base+".") {
		return
	}
	o.note("$health-passive", Manual, "source clears failure history after success and holds exclusion for a full window; native failures age individually without success reset and all-demoted pools use degraded forwarding")
}
