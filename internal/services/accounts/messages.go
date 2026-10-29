package accounts

import (
	"github.com/gocql/gocql"

	pb "github.com/ssenerg/gochat/api/protobuf/gen/gochat/v1"
	"github.com/ssenerg/gochat/internal/core/models"
)

type SignupInfo struct{ InviteToken string }

type SignupInfoResult struct {
	Bootstrap          bool
	SetupTokenRequired bool
	// SetupClosed is set while no account exists and none can be created:
	// the server has no setup token.
	SetupClosed     bool
	Open            bool
	InviteValid     bool
	TeamDisplayName string
}

type SignUp struct {
	Username, Email, DisplayName, Password, InviteToken, SetupToken string
	IP, UserAgent                                                   string
}

type SignUpResult struct {
	Session *pb.Session
	User    *models.User
	// InviteTeamID is the team the consumed invite admits the user to, and
	// InviteHash that invite's.
	InviteTeamID gocql.UUID
	InviteHash   []byte
}

type Login struct {
	Login, Password, IP, UserAgent string
}

type LoginResult struct {
	Session  *pb.Session
	MFAToken string
}

type VerifyMFA struct {
	Token, Code, IP, UserAgent string
}

// Authenticate resolves a session token; IP, when known, is where the
// request came from.
type Authenticate struct{ Token, IP string }

type Logout struct{ P *models.Principal }

type ChangePassword struct {
	P                    *models.Principal
	Current, NewPassword string
}

type BeginTOTP struct {
	P        *models.Principal
	Password string
}

type TOTPSetup struct{ Secret, URL string }

type ConfirmTOTP struct {
	P    *models.Principal
	Code string
}

type RecoveryCodes []string

type DisableTOTP struct {
	P              *models.Principal
	Password, Code string
}

type Done struct{}

// DeleteAccount checks P may delete their account: their password, or for
// an account without one a sign-in at its provider that just finished, and
// their second factor. It refuses the last system admin, and suspends the
// account it lets through until EraseAccount or ResumeAccount.
type DeleteAccount struct {
	P                                     *models.Principal
	Password, Code, SSOToken, SSOVerifier string
}

// EraseAccount deletes the account of P, which DeleteAccount let through,
// once it left its teams, and tells those of Tell it is gone.
type EraseAccount struct {
	P    *models.Principal
	Tell []gocql.UUID
}

// ResumeAccount lifts the suspension of a deletion that failed.
type ResumeAccount struct{ P *models.Principal }

// ListSessions replies with the []*pb.SessionInfo of the caller, or of
// UserID for someone who may manage any.
type ListSessions struct {
	P      *models.Principal
	UserID string
}

// RevokeSession signs out one of the caller's sessions.
type RevokeSession struct {
	P         *models.Principal
	SessionID string
}

// RevokeSessions signs out the caller's other sessions, or every session of
// UserID, and replies with how many.
type RevokeSessions struct {
	P      *models.Principal
	UserID string
}

type Revoked int

// FindUser replies with the *models.User of UserID, or else of Login, a
// username or an email.
type FindUser struct {
	P             *models.Principal
	Login, UserID string
}

// SetDeactivated deactivates or reactivates a user and replies with the
// *models.User.
type SetDeactivated struct {
	P           *models.Principal
	UserID      string
	Deactivated bool
}

// SSOProviders replies with the []SSOProviderInfo people may sign in with.
type SSOProviders struct{}

type SSOProviderInfo struct{ ID, Name string }

// BeginSSO starts a sign-in at a provider, which comes back to RedirectURI.
// P, when set, links the identity to P's account instead; AppChallenge
// hands the sign-in to the app holding its PKCE verifier. Reauth only
// proves who signs in, for DeleteAccount.
type BeginSSO struct {
	Provider, RedirectURI, AppChallenge string
	Reauth                              bool
	P                                   *models.Principal
}

type SSOStart struct{ URL, State string }

// FinishSSO finishes a sign-in with the code the provider gave. P is
// whoever is signed in where it finishes, and InviteToken the invite they
// came with.
type FinishSSO struct {
	State, Code, InviteToken string
	P                        *models.Principal
	IP, UserAgent            string
}

// SSOResult is one of: a sign-in, an app's code, a link to confirm, or a
// link done.
type SSOResult struct {
	Login   *LoginResult
	AppCode string
	// LinkToken is set when the identity is new and its email has an
	// account: its owner signs in with their password, then links it.
	LinkToken string
	Linked    bool
	// ReauthToken proves who signed in, for a reauth begun with a session;
	// an app's comes as its AppCode.
	ReauthToken string
	// Provider is the provider's name.
	Provider string
	// User, InviteTeamID and InviteHash are of a new account and its invite.
	User         *models.User
	InviteTeamID gocql.UUID
	InviteHash   []byte
}

// RedeemSSO turns an app's code into a sign-in, given the verifier of the
// challenge it began with.
type RedeemSSO struct {
	Code, Verifier, IP, UserAgent string
}

// GetSSOLink replies with the *SSOLinkInfo of a link to confirm.
type GetSSOLink struct{ Token string }

type SSOLinkInfo struct{ Provider, Email string }

// LinkSSO links the identity of a link token to the caller's account and
// replies with the *SSOLinkInfo.
type LinkSSO struct {
	P     *models.Principal
	Token string
}

// ListSSOIdentities replies with the []*pb.SSOIdentity of the caller, or
// of UserID for someone who may find users.
type ListSSOIdentities struct {
	P      *models.Principal
	UserID string
}

// UnlinkSSO unlinks an identity from the caller, or from UserID for someone
// who may deactivate users.
type UnlinkSSO struct {
	P                       *models.Principal
	UserID, Issuer, Subject string
}

// ListSSOAccounts replies with the *pb.ListSSOAccountsResponse of a
// provider, a page of the accounts linked through it after the user After.
type ListSSOAccounts struct {
	P               *models.Principal
	Provider, After string
	Limit           int
}
