package store

import "context"

// Actors: who made a change, recorded on every task event (schema v7) and in
// the orchd ledger. The form is "kind:id" — "socket:uid:1000",
// "session:mac-passkey", "token:mac-ssh" — or "system" for the daemon itself
// (worker steps, crash recovery). The API attaches the actor to the request
// context in one place (api.withPrincipal); event writers read it with
// ActorFrom, so no function signature carries it.
const (
	// ActorSystem is the actor of a change no authenticated caller made.
	ActorSystem = "system"
	// ActorInvalid is recorded instead of an actor string that breaks the
	// format: the raw text is never stored, and the write never fails.
	ActorInvalid = "invalid"
	// MaxActorLen caps an actor string in bytes.
	MaxActorLen = 80
)

type actorKey struct{}

// WithActor returns ctx carrying actor (normalised when read, see ActorFrom).
func WithActor(ctx context.Context, actor string) context.Context {
	return context.WithValue(ctx, actorKey{}, actor)
}

// ActorFrom returns the actor carried by ctx: ActorSystem when there is none,
// ActorInvalid when the carried string breaks the format (NormalizeActor).
func ActorFrom(ctx context.Context) string {
	if ctx == nil {
		return ActorSystem
	}
	a, ok := ctx.Value(actorKey{}).(string)
	if !ok {
		return ActorSystem
	}
	return NormalizeActor(a)
}

// NormalizeActor returns a unchanged when it is a valid actor and
// ActorInvalid otherwise. Valid: 1..MaxActorLen bytes of [A-Za-z0-9_.:@-],
// and either "system" or "kind:id" with a non-empty kind and id.
func NormalizeActor(a string) string {
	if a == ActorSystem {
		return a
	}
	if len(a) == 0 || len(a) > MaxActorLen {
		return ActorInvalid
	}
	colon := -1
	for i := 0; i < len(a); i++ {
		c := a[i]
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9',
			c == '_', c == '.', c == '@', c == '-':
		case c == ':':
			if colon < 0 {
				colon = i
			}
		default:
			return ActorInvalid
		}
	}
	if colon <= 0 || colon == len(a)-1 {
		return ActorInvalid
	}
	return a
}
