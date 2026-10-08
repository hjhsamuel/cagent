package config

// Capacity 是本实例硬上限；等待远端任务的 Run 也占运行槽位，避免长任务积累
// 无界 goroutine。远端任务没有 TTL；本实例满载时恢复候选留待后续扫描。
type Capacity struct{ Runs, Models, Observations int }

func (c Capacity) Validate() error {
	for _, v := range []struct {
		name string
		n    int
	}{{"capacity.runs", c.Runs}, {"capacity.models", c.Models}, {"capacity.observations", c.Observations}} {
		if v.n < 1 || v.n > 100000 {
			return invalid(v.name, "must be between 1 and 100000")
		}
	}
	return nil
}
