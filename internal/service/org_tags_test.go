package service

import (
	"reflect"
	"testing"
)

func TestOrgTagMembershipUsesExactValues(t *testing.T) {
	if hasOrgTag("TEAM_10", "TEAM_1") {
		t.Fatal("substring must not grant organization membership")
	}
	if !hasOrgTag(" TEAM_1,TEAM_10 ", "TEAM_1") {
		t.Fatal("exact organization tag should be recognized")
	}
	if got, want := parseOrgTags(" TEAM_1, ,TEAM_10 "), []string{"TEAM_1", "TEAM_10"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("parseOrgTags() = %v, want %v", got, want)
	}
}
