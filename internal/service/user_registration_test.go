package service

import (
	"context"
	"errors"
	"pai-smart-go/internal/model"
	"pai-smart-go/internal/repository"
	"strings"
	"testing"

	"gorm.io/gorm"
)

type registrationRepo struct {
	repository.UserRepository
	user *model.User
	tag  *model.OrganizationTag
	err  error
}

func (r *registrationRepo) FindByUsername(context.Context, string) (*model.User, error) {
	return nil, gorm.ErrRecordNotFound
}

func (r *registrationRepo) CreateWithPrivateTag(user *model.User, tag *model.OrganizationTag) error {
	r.user, r.tag = user, tag
	return r.err
}

func TestRegisterUsesAtomicUserAndPrivateTagWrite(t *testing.T) {
	repo := &registrationRepo{}
	user, err := NewUserService(repo, nil, nil).Register("alice", "password")
	if err != nil {
		t.Fatal(err)
	}
	if user != repo.user || repo.tag == nil || user.OrgTags != "PRIVATE_alice" || user.PrimaryOrg != repo.tag.TagID {
		t.Fatalf("registration was not staged atomically: user=%#v tag=%#v", user, repo.tag)
	}

	repo.err = errors.New("tag collision")
	if user, err = NewUserService(repo, nil, nil).Register("bob", "password"); err == nil || user != nil {
		t.Fatal("transaction failure returned a registered user")
	}
}

func TestRegisterRejectsUsernameThatCanInjectOrgTags(t *testing.T) {
	repo := &registrationRepo{}
	for _, username := range []string{"x,PRIVATE_admin", " leading", "line\nbreak", strings.Repeat("a", 33)} {
		if user, err := NewUserService(repo, nil, nil).Register(username, "password"); err == nil || user != nil {
			t.Fatalf("unsafe username %q was accepted", username)
		}
	}
	if repo.user != nil || repo.tag != nil {
		t.Fatal("invalid username reached persistence")
	}
}
