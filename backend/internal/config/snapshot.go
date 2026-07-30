package config

// Snapshot is an immutable, validated view of the config plus a precomputed
// model->vendors index. All accessors return values or copies so callers can
// never mutate the shared internal state.
type Snapshot struct {
	settings Settings
	vendors  []Vendor

	byName  map[string]int      // vendor name -> index into vendors
	byModel map[string][]Vendor // model -> vendors serving it, ordered as declared
}

// newSnapshot builds the index from an already-validated config. It takes
// ownership of cfg's slices, so callers must not retain or mutate cfg after.
func newSnapshot(cfg Config) *Snapshot {
	s := &Snapshot{
		settings: cfg.Settings,
		vendors:  cfg.Vendors,
		byName:   make(map[string]int, len(cfg.Vendors)),
		byModel:  make(map[string][]Vendor),
	}
	for i := range s.vendors {
		v := s.vendors[i]
		s.byName[v.Name] = i
		for _, m := range v.ServedModels {
			routed := v
			if route, ok := v.ModelRoutes[m]; ok {
				if !route.Enabled {
					continue
				}
				routed.Priority = route.Priority
				routed.Weight = route.Weight
			}
			s.byModel[m] = append(s.byModel[m], routed)
		}
	}
	return s
}

// emptySnapshot returns a valid Snapshot with no vendors, used for first-run
// (missing file) startup.
func emptySnapshot() *Snapshot {
	return newSnapshot(Config{})
}

// Settings returns the gateway settings.
func (s *Snapshot) Settings() Settings { return s.settings }

// Vendors returns a copy of the vendor list.
func (s *Snapshot) Vendors() []Vendor {
	out := make([]Vendor, len(s.vendors))
	for i := range s.vendors {
		out[i] = cloneVendor(s.vendors[i])
	}
	return out
}

// Vendor returns the vendor with the given name.
func (s *Snapshot) Vendor(name string) (Vendor, bool) {
	i, ok := s.byName[name]
	if !ok {
		return Vendor{}, false
	}
	return cloneVendor(s.vendors[i]), true
}

// VendorsForModel returns every vendor whose served_models contains model.
// It is a cheap map lookup against the precomputed index.
func (s *Snapshot) VendorsForModel(model string) []Vendor {
	vs := s.byModel[model]
	if len(vs) == 0 {
		return nil
	}
	out := make([]Vendor, len(vs))
	for i := range vs {
		out[i] = cloneVendor(vs[i])
	}
	return out
}

// PriceFor returns the price for a model under a specific vendor.
func (s *Snapshot) PriceFor(vendorName, model string) (Price, bool) {
	i, ok := s.byName[vendorName]
	if !ok {
		return Price{}, false
	}
	p, ok := s.vendors[i].Prices[model]
	return p, ok
}

// cloneVendor deep-copies the slices and map of a Vendor so a returned value
// shares no mutable state with the Snapshot.
func cloneVendor(v Vendor) Vendor {
	out := v
	if v.Proxy != nil {
		p := *v.Proxy
		out.Proxy = &p
	}
	if v.ServedModels != nil {
		out.ServedModels = append([]string(nil), v.ServedModels...)
	}
	if v.ModelRoutes != nil {
		out.ModelRoutes = make(map[string]ModelRoute, len(v.ModelRoutes))
		for k, route := range v.ModelRoutes {
			out.ModelRoutes[k] = route
		}
	}
	if v.Prices != nil {
		out.Prices = make(map[string]Price, len(v.Prices))
		for k, p := range v.Prices {
			out.Prices[k] = p
		}
	}
	if v.Wires != nil {
		out.Wires = append([]string(nil), v.Wires...)
	}
	if v.Endpoints != nil {
		out.Endpoints = make(map[string]string, len(v.Endpoints))
		for k, endpoint := range v.Endpoints {
			out.Endpoints[k] = endpoint
		}
	}
	if v.Quirks != nil {
		out.Quirks = make(map[string]string, len(v.Quirks))
		for k, value := range v.Quirks {
			out.Quirks[k] = value
		}
	}
	return out
}
