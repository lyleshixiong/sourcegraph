package graphqlbackend

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	graphql "github.com/neelance/graphql-go"
	"github.com/neelance/graphql-go/relay"
	log15 "gopkg.in/inconshreveable/log15.v2"

	"sourcegraph.com/sourcegraph/sourcegraph/cmd/frontend/internal/app/envvar"
	"sourcegraph.com/sourcegraph/sourcegraph/cmd/frontend/internal/app/invite"
	"sourcegraph.com/sourcegraph/sourcegraph/cmd/frontend/internal/app/slack"
	"sourcegraph.com/sourcegraph/sourcegraph/cmd/frontend/internal/globals"
	"sourcegraph.com/sourcegraph/sourcegraph/pkg/auth0"
	"sourcegraph.com/sourcegraph/sourcegraph/pkg/backend"
	"sourcegraph.com/sourcegraph/sourcegraph/pkg/conf"
	"sourcegraph.com/sourcegraph/sourcegraph/pkg/db"

	"sourcegraph.com/sourcegraph/sourcegraph/pkg/actor"
	sourcegraph "sourcegraph.com/sourcegraph/sourcegraph/pkg/api"
	"sourcegraph.com/sourcegraph/sourcegraph/pkg/db"
)

func (r *schemaResolver) Org(ctx context.Context, args *struct {
	ID graphql.ID
}) (*orgResolver, error) {
	return orgByID(ctx, args.ID)
}

func orgByID(ctx context.Context, id graphql.ID) (*orgResolver, error) {
	orgID, err := unmarshalOrgID(id)
	if err != nil {
		return nil, err
	}
	return orgByIDInt32(ctx, orgID)
}

func orgByIDInt32(ctx context.Context, orgID int32) (*orgResolver, error) {
	// 🚨 SECURITY: Check that the current user is a member of the org.
	if err := backend.CheckCurrentUserIsOrgMember(ctx, orgID); err != nil {
		return nil, err
	}

	org, err := db.Orgs.GetByID(ctx, orgID)
	if err != nil {
		return nil, err
	}
	return &orgResolver{org}, nil
}

type orgResolver struct {
	org *sourcegraph.Org
}

func (o *orgResolver) ID() graphql.ID { return marshalOrgID(o.org.ID) }

func marshalOrgID(id int32) graphql.ID { return relay.MarshalID("Org", id) }

func unmarshalOrgID(id graphql.ID) (orgID int32, err error) {
	err = relay.UnmarshalSpec(id, &orgID)
	return
}

func (o *orgResolver) OrgID() int32 {
	return o.org.ID
}

func (o *orgResolver) Name() string {
	return o.org.Name
}

func (o *orgResolver) DisplayName() *string {
	return o.org.DisplayName
}

func (o *orgResolver) SlackWebhookURL() *string {
	return o.org.SlackWebhookURL
}

func (o *orgResolver) CreatedAt() string { return o.org.CreatedAt.Format(time.RFC3339) }

func (o *orgResolver) Members(ctx context.Context) ([]*orgMemberResolver, error) {
	sgMembers, err := db.OrgMembers.GetByOrgID(ctx, o.org.ID)
	if err != nil {
		return nil, err
	}

	members := []*orgMemberResolver{}
	for _, sgMember := range sgMembers {
		member := &orgMemberResolver{o.org, sgMember, nil}
		members = append(members, member)
	}
	return members, nil
}

func (o *orgResolver) LatestSettings(ctx context.Context) (*settingsResolver, error) {
	settings, err := db.Settings.GetLatest(ctx, sourcegraph.ConfigurationSubject{Org: &o.org.ID})
	if err != nil {
		return nil, err
	}
	if settings == nil {
		return nil, nil
	}
	return &settingsResolver{&configurationSubject{org: o}, settings, nil}, nil
}

func (o *orgResolver) Threads(ctx context.Context, args *struct {
	RepoCanonicalRemoteID *string // TODO(nick): deprecated
	CanonicalRemoteIDs    *[]string
	Branch                *string
	File                  *string
	Limit                 *int32
}) (*threadConnectionResolver, error) {
	var canonicalRemoteIDs []string
	if args.CanonicalRemoteIDs != nil {
		canonicalRemoteIDs = append(canonicalRemoteIDs, *args.CanonicalRemoteIDs...)
	}
	if args.RepoCanonicalRemoteID != nil {
		canonicalRemoteIDs = append(canonicalRemoteIDs, *args.RepoCanonicalRemoteID)
	}
	var repos []*sourcegraph.OrgRepo
	if len(canonicalRemoteIDs) > 0 {
		var err error
		repos, err = db.OrgRepos.GetByCanonicalRemoteIDs(ctx, o.org.ID, canonicalRemoteIDs)
		if err != nil {
			return nil, err
		}
	}
	return &threadConnectionResolver{o.org, repos, canonicalRemoteIDs, args.File, args.Branch, args.Limit}, nil
}

func (o *orgResolver) Tags(ctx context.Context) ([]*orgTagResolver, error) {
	tags, err := db.OrgTags.GetByOrgID(ctx, o.org.ID)
	if err != nil {
		return nil, err
	}
	orgTagResolvers := []*orgTagResolver{}
	for _, tag := range tags {
		orgTagResolvers = append(orgTagResolvers, &orgTagResolver{tag})
	}
	return orgTagResolvers, nil
}

func (o *orgResolver) Repo(ctx context.Context, args *struct {
	CanonicalRemoteID string
}) (*orgRepoResolver, error) {
	orgRepo, err := getOrgRepo(ctx, o.org.ID, args.CanonicalRemoteID)
	if err != nil {
		return nil, err
	}
	return &orgRepoResolver{o.org, orgRepo}, nil
}

func getOrgRepo(ctx context.Context, orgID int32, canonicalRemoteID string) (*sourcegraph.OrgRepo, error) {
	orgRepo, err := db.OrgRepos.GetByCanonicalRemoteID(ctx, orgID, canonicalRemoteID)
	if err == db.ErrRepoNotFound {
		// We don't want to create org repos just because an org member queried for threads
		// and we don't want the client to think this is an error.
		err = nil
	}
	return orgRepo, err
}

func (o *orgResolver) Repos(ctx context.Context) ([]*orgRepoResolver, error) {
	repos, err := db.OrgRepos.GetByOrg(ctx, o.org.ID)
	if err != nil {
		return nil, err
	}
	orgRepoResolvers := []*orgRepoResolver{}
	for _, repo := range repos {
		orgRepoResolvers = append(orgRepoResolvers, &orgRepoResolver{o.org, repo})
	}
	return orgRepoResolvers, nil
}

func (*schemaResolver) CreateOrg(ctx context.Context, args *struct {
	Name        string
	DisplayName string
}) (*orgResolver, error) {
	currentUser, err := currentUser(ctx)
	if err != nil {
		return nil, err
	}
	if currentUser == nil {
		return nil, errors.New("no current user")
	}

	newOrg, err := db.Orgs.Create(ctx, args.Name, args.DisplayName)
	if err != nil {
		return nil, err
	}

	// Add the current user as the first member of the new org.
	_, err = db.OrgMembers.Create(ctx, newOrg.ID, *currentUser.SourcegraphID())
	if err != nil {
		return nil, err
	}

	{
		// Orgs created by an editor-beta user get the editor-beta tag.
		//
		// TODO(sqs): perform this transactionally with the other operations above.
		const editorBetaTag = "editor-beta"
		tag, err := db.UserTags.GetByUserIDAndTagName(ctx, *currentUser.SourcegraphID(), editorBetaTag)
		if _, ok := err.(db.ErrUserTagNotFound); !ok && err != nil {
			return nil, err
		} else if tag != nil {
			if _, err = db.OrgTags.Create(ctx, newOrg.ID, editorBetaTag); err != nil {
				return nil, err
			}
		}
	}

	return &orgResolver{org: newOrg}, nil
}

func (*schemaResolver) UpdateOrg(ctx context.Context, args *struct {
	ID              graphql.ID
	DisplayName     *string
	SlackWebhookURL *string
}) (*orgResolver, error) {
	var orgID int32
	if err := relay.UnmarshalSpec(args.ID, &orgID); err != nil {
		return nil, err
	}

	// 🚨 SECURITY: Check that the current user is a member
	// of the org that is being modified.
	if err := backend.CheckCurrentUserIsOrgMember(ctx, orgID); err != nil {
		return nil, err
	}

	log15.Info("updating org", "org", args.ID, "display name", args.DisplayName, "webhook URL", args.SlackWebhookURL)

	updatedOrg, err := db.Orgs.Update(ctx, orgID, args.DisplayName, args.SlackWebhookURL)
	if err != nil {
		return nil, err
	}

	return &orgResolver{org: updatedOrg}, nil
}

func (*schemaResolver) RemoveUserFromOrg(ctx context.Context, args *struct {
	UserID int32
	OrgID  graphql.ID
}) (*EmptyResponse, error) {
	var orgID int32
	if err := relay.UnmarshalSpec(args.OrgID, &orgID); err != nil {
		return nil, err
	}

	// 🚨 SECURITY: Check that the current user is a member
	// of the org that is being modified.
	if err := backend.CheckCurrentUserIsOrgMember(ctx, orgID); err != nil {
		return nil, err
	}

	log15.Info("removing user from org", "user", args.UserID, "org", orgID)
	return nil, db.OrgMembers.Remove(ctx, orgID, args.UserID)
}

type inviteUserResult struct {
	acceptInviteURL string
}

func (r *inviteUserResult) AcceptInviteURL() string { return r.acceptInviteURL }

func (*schemaResolver) InviteUser(ctx context.Context, args *struct {
	OrgID graphql.ID
	Email string
}) (*inviteUserResult, error) {
	var orgID int32
	if err := relay.UnmarshalSpec(args.OrgID, &orgID); err != nil {
		return nil, err
	}

	// 🚨 SECURITY: Check that the current user is a member
	// of the org that the user is being invited to.
	if err := backend.CheckCurrentUserIsOrgMember(ctx, orgID); err != nil {
		return nil, err
	}

	currentUser, err := currentUser(ctx)
	if err != nil {
		return nil, err
	}
	if currentUser == nil {
		return nil, errors.New("must be logged in")
	}

	// Don't invite the user if they are already a member.
	invitedUser, err := db.Users.GetByEmail(ctx, args.Email)
	if err != nil {
		if _, ok := err.(db.ErrUserNotFound); !ok {
			return nil, err
		}
	}

	if invitedUser != nil {
		_, err = db.OrgMembers.GetByOrgIDAndUserID(ctx, orgID, invitedUser.ID)
		if err == nil {
			return nil, fmt.Errorf("%s is already a member of org %d", args.Email, orgID)
		}
		if _, ok := err.(db.ErrOrgMemberNotFound); !ok {
			return nil, err
		}
	}

	if !envvar.DeploymentOnPrem() {
		// Only allow email-verified users to send invites.
		if emailVerified, err := auth0.GetEmailVerificationStatus(ctx); err != nil {
			return nil, err
		} else if !emailVerified && strings.HasPrefix(currentUser.AuthID(), "auth0|") {
			return nil, errors.New("must verify your email to send invites")
		}

		// TODO(sqs): check email verif for non-auth0 users

		// Check and decrement our invite quota, to prevent abuse (sending too many invites).
		//
		// There is no user invite quota for on-prem instances because we assume they can
		// trust their users to not abuse invites.
		if err := db.Users.CheckAndDecrementInviteQuota(ctx, *currentUser.SourcegraphID()); err != nil {
			if err == db.ErrInviteQuotaExceeded {
				return nil, fmt.Errorf("%s (contact support to increase the quota)", err)
			}
			return nil, err
		}
	}

	org, err := db.Orgs.GetByID(ctx, orgID)
	if err != nil {
		return nil, err
	}

	if invitedUser != nil {
		// If the org has the editor-beta tag, add the editor beta tag to an invited user
		// if they're already registered.
		const editorBetaTag = "editor-beta"
		tag, err := db.OrgTags.GetByOrgIDAndTagName(ctx, org.ID, editorBetaTag)
		if _, ok := err.(db.ErrOrgTagNotFound); !ok && err != nil {
			return nil, err
		} else if tag != nil {
			if _, err = db.UserTags.Create(ctx, invitedUser.ID, editorBetaTag); err != nil {
				return nil, err
			}
		}
	}

	token, err := invite.CreateOrgToken(args.Email, org)
	if err != nil {
		return nil, err
	}

	inviteURL := globals.AppURL.String() + "/settings/accept-invite?token=" + token

	if conf.CanSendEmail() {
		// If email is disabled, the frontend will show a link instead.
		invite.SendEmail(args.Email, *currentUser.DisplayName(), org.Name, inviteURL)
	}

	client := slack.New(org.SlackWebhookURL, true)
	go client.NotifyOnInvite(currentUser, org, args.Email)

	return &inviteUserResult{acceptInviteURL: inviteURL}, nil
}

func (*schemaResolver) AcceptUserInvite(ctx context.Context, args *struct {
	InviteToken string
}) (*orgInviteResolver, error) {
	currentUser, err := currentUser(ctx)
	if err != nil {
		return nil, err
	}
	if currentUser == nil {
		return nil, errors.New("no current user")
	}

	// If the user is natively authenticated, require a verified email (if via SSO, we assume the SSO provider
	// has authenticated the user's email)
	if actor := actor.FromContext(ctx); actor.Provider == "" {
		u, err := auth0.GetAuth0User(ctx)
		if err != nil {
			return nil, err
		}
		if !u.EmailVerified && !envvar.DeploymentOnPrem() && strings.HasPrefix(actor.UID, "auth0|") {
			// Don't add user to the org until email is verified. This will be a common failure mode,
			// so rather than return an error we return a response the client can handle.
			// Email verification is only a requirement for Sourcegraph.com.
			return &orgInviteResolver{emailVerified: false}, nil
		}
	}

	token, err := invite.ParseToken(args.InviteToken)
	if err != nil {
		return nil, err
	}
	org, err := db.Orgs.GetByID(ctx, token.OrgID)
	if err != nil {
		return nil, err
	}

	_, err = db.OrgMembers.Create(ctx, token.OrgID, *currentUser.SourcegraphID())
	if err != nil {
		return nil, err
	}

	client := slack.New(org.SlackWebhookURL, true)
	go client.NotifyOnAcceptedInvite(currentUser, org)

	return &orgInviteResolver{emailVerified: true}, nil
}

// unmarshalOrgGraphQLID unmarshals and returns the int32 org ID of the first
// non-nil element of ids.
func unmarshalOrgGraphQLID(ids ...*graphql.ID) (int32, error) {
	for _, id := range ids {
		if id != nil {
			var orgID int32
			err := relay.UnmarshalSpec(*id, &orgID)
			return orgID, err
		}
	}
	return 0, errors.New("at least 1 of id and orgID must be specified")
}
