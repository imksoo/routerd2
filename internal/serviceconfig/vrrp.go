package serviceconfig

import "fmt"

func validateVRRPTracks(services []Service) error {
	health := make(map[string]bool)
	for _, service := range services {
		if service.Health != nil {
			health[service.Name] = true
		}
	}
	for _, service := range services {
		if service.VRRP == nil {
			continue
		}
		for _, track := range service.VRRP.Track {
			if !health[track.Service] {
				return fmt.Errorf("VRRP %s track source %s is not a configured Health service", service.Name, track.Service)
			}
		}
	}
	return nil
}
