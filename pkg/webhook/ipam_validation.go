package webhook

import (
	"encoding/json"
	"fmt"
	"strings"

	netutils "k8s.io/utils/net"
)

const whereaboutsIPAMType = "whereabouts"

func validateIPAMConfigs(config []byte) error {
	var c map[string]interface{}
	if err := json.Unmarshal(config, &c); err != nil {
		return fmt.Errorf("invalid json: %w", err)
	}

	if plugins, ok := c["plugins"].([]interface{}); ok {
		for _, plugin := range plugins {
			pluginConfig, ok := plugin.(map[string]interface{})
			if !ok {
				return fmt.Errorf("invalid plugin config")
			}
			if err := validatePluginIPAM(pluginConfig); err != nil {
				return err
			}
		}
		return nil
	}

	return validatePluginIPAM(c)
}

func validatePluginIPAM(plugin map[string]interface{}) error {
	ipamRaw, ok := plugin["ipam"]
	if !ok {
		return nil
	}

	ipam, ok := ipamRaw.(map[string]interface{})
	if !ok {
		return fmt.Errorf("invalid ipam config")
	}

	ipamType, _ := ipam["type"].(string)
	if ipamType != whereaboutsIPAMType {
		return nil
	}

	return validateWhereaboutsIPAM(ipam)
}

func validateWhereaboutsIPAM(ipam map[string]interface{}) error {
	if raw, ok := ipam["range"]; ok {
		rangeStr, ok := raw.(string)
		if !ok {
			return fmt.Errorf("invalid whereabouts ipam range: must be a string")
		}
		if rangeStr != "" {
			if err := validateWhereaboutsRange(rangeStr); err != nil {
				return fmt.Errorf("invalid whereabouts ipam range: %w", err)
			}
		}
	}

	if err := validateWhereaboutsStringIP(ipam, "range_start"); err != nil {
		return err
	}
	if err := validateWhereaboutsStringIP(ipam, "range_end"); err != nil {
		return err
	}
	if err := validateWhereaboutsStringIP(ipam, "gateway"); err != nil {
		return err
	}

	if raw, ok := ipam["exclude"]; ok {
		if err := validateWhereaboutsExcludeList(raw); err != nil {
			return err
		}
	}

	rawIPRanges, ok := ipam["ipRanges"]
	if !ok {
		return nil
	}

	ipRangesRaw, ok := rawIPRanges.([]interface{})
	if !ok {
		return fmt.Errorf("invalid whereabouts ipam ipRanges: must be an array")
	}

	for idx, ipRangeRaw := range ipRangesRaw {
		ipRange, ok := ipRangeRaw.(map[string]interface{})
		if !ok {
			return fmt.Errorf("invalid whereabouts ipam ipRanges entry at index %d", idx)
		}

		if err := validateWhereaboutsIPRangeEntry(ipRange, idx); err != nil {
			return err
		}
	}

	return nil
}

func validateWhereaboutsIPRangeEntry(ipRange map[string]interface{}, idx int) error {
	if raw, ok := ipRange["range"]; ok {
		rangeStr, ok := raw.(string)
		if !ok {
			return fmt.Errorf("invalid whereabouts ipam ipRanges[%d].range: must be a string", idx)
		}
		if rangeStr != "" {
			if err := validateWhereaboutsRange(rangeStr); err != nil {
				return fmt.Errorf("invalid whereabouts ipam ipRanges[%d].range: %w", idx, err)
			}
		}
	}

	if err := validateWhereaboutsStringIP(ipRange, "range_start"); err != nil {
		return fmt.Errorf("invalid whereabouts ipam ipRanges[%d].range_start: %w", idx, err)
	}
	if err := validateWhereaboutsStringIP(ipRange, "range_end"); err != nil {
		return fmt.Errorf("invalid whereabouts ipam ipRanges[%d].range_end: %w", idx, err)
	}

	if raw, ok := ipRange["exclude"]; ok {
		if err := validateWhereaboutsExcludeList(raw); err != nil {
			return fmt.Errorf("invalid whereabouts ipam ipRanges[%d].exclude: %w", idx, err)
		}
	}

	return nil
}

func validateWhereaboutsExcludeList(excludeRaw interface{}) error {
	excludeList, ok := excludeRaw.([]interface{})
	if !ok {
		return fmt.Errorf("invalid whereabouts ipam exclude: must be an array")
	}
	if len(excludeList) == 0 {
		return nil
	}

	for idx, excludeEntry := range excludeList {
		excludeStr, ok := excludeEntry.(string)
		if !ok {
			return fmt.Errorf("invalid exclude entry at index %d", idx)
		}
		if err := validateWhereaboutsRange(excludeStr); err != nil {
			return fmt.Errorf("invalid CIDR in exclude list %s: %w", excludeStr, err)
		}
	}

	return nil
}

func validateWhereaboutsStringIP(ipam map[string]interface{}, field string) error {
	raw, ok := ipam[field]
	if !ok {
		return nil
	}

	value, ok := raw.(string)
	if !ok {
		return fmt.Errorf("invalid whereabouts ipam %s: must be a string", field)
	}
	if value == "" {
		return nil
	}

	if netutils.ParseIPSloppy(value) == nil {
		return fmt.Errorf("invalid whereabouts ipam %s: %s", field, value)
	}

	return nil
}

func validateWhereaboutsRange(rangeStr string) error {
	parts := strings.SplitN(rangeStr, "-", 2)
	if len(parts) == 2 {
		if netutils.ParseIPSloppy(strings.TrimSpace(parts[0])) == nil {
			return fmt.Errorf("invalid range start IP: %s", parts[0])
		}
		if _, _, err := netutils.ParseCIDRSloppy(strings.TrimSpace(parts[1])); err != nil {
			return fmt.Errorf("invalid CIDR '%s': %w", parts[1], err)
		}
		return nil
	}

	if _, _, err := netutils.ParseCIDRSloppy(rangeStr); err != nil {
		return fmt.Errorf("invalid CIDR %s: %w", rangeStr, err)
	}

	return nil
}
