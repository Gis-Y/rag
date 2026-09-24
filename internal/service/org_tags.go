package service

import "strings"

func parseOrgTags(raw string) []string {
	if raw == "" {
		return []string{}
	}
	tags := make([]string, 0)
	for _, tag := range strings.Split(raw, ",") {
		if tag = strings.TrimSpace(tag); tag != "" {
			tags = append(tags, tag)
		}
	}
	return tags
}

func hasOrgTag(raw, target string) bool {
	for _, tag := range parseOrgTags(raw) {
		if tag == target {
			return true
		}
	}
	return false
}
