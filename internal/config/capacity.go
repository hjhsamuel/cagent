package config

// Capacity bounds active generation, model requests and observations separately.
// Waiting tasks release Runs capacity; Maintenance has its own worker bound.
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
