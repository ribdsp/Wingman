package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/rs/zerolog"

	"github.com/ribdsp/wingman/core/internal/repository"
)

// ChatsDeps is everything the chat service needs.
type ChatsDeps struct {
	Chats    ChatStore
	Messages MessageStore

	Clock  Clock
	Logger zerolog.Logger
}

// Chats is a person's conversations: making them, listing them, renaming them, putting
// them away, and reading what was said in one.
//
// It does not start work. Sending a message is Tasks.Send, because a message is the
// beginning of a run and a run is bounded by budgets this service knows nothing about —
// which keeps this one to plain, cheap CRUD that a client can call freely.
//
// Every method scopes by the actor's own user id, taken from the actor rather than from
// the request. There is no operator view of somebody else's chats, and that absence is
// deliberate: an operator can read the transcript of a run, which is what they need to
// audit what the agent did, without reading the conversation around it.
type Chats struct {
	chats    ChatStore
	messages MessageStore

	clock Clock
	log   zerolog.Logger
}

// NewChats validates its wiring and returns a ready service.
func NewChats(deps ChatsDeps) (*Chats, error) {
	missing := []string{}
	require := func(ok bool, name string) {
		if !ok {
			missing = append(missing, name)
		}
	}
	require(deps.Chats != nil, "Chats")
	require(deps.Messages != nil, "Messages")
	if len(missing) > 0 {
		return nil, fmt.Errorf("chats: missing dependencies: %v", missing)
	}

	c := &Chats{
		chats:    deps.Chats,
		messages: deps.Messages,
		clock:    deps.Clock,
		log:      deps.Logger,
	}
	if c.clock == nil {
		c.clock = time.Now
	}
	return c, nil
}

// Create opens an empty chat.
//
// An empty title is allowed and becomes a placeholder, because a client that opens a
// chat before the person has typed anything has nothing to name it after yet.
func (c *Chats) Create(ctx context.Context, title string, actor Actor) (repository.Chat, error) {
	userID, err := c.owner(actor)
	if err != nil {
		return repository.Chat{}, err
	}

	clean, problem := checkTitle(title, false)
	if problem != "" {
		return repository.Chat{}, titleProblem(problem)
	}
	return c.chats.CreateChat(ctx, userID, clean)
}

// Get is one of the caller's own chats.
func (c *Chats) Get(ctx context.Context, chatID string, actor Actor) (repository.Chat, error) {
	userID, err := c.owner(actor)
	if err != nil {
		return repository.Chat{}, err
	}
	if strings.TrimSpace(chatID) == "" {
		return repository.Chat{}, fmt.Errorf("%w: a chat id is required", ErrValidation)
	}

	chat, err := c.chats.GetChat(ctx, userID, chatID)
	if err != nil {
		return repository.Chat{}, mapAbsence(err)
	}
	return chat, nil
}

// List is the caller's chats, most recently active first.
//
// Archived chats are excluded unless asked for. Archiving is how somebody clears a list
// they have to read, so honouring it by default is the whole point of the field.
func (c *Chats) List(ctx context.Context, includeArchived bool, limit, offset int, actor Actor) ([]repository.Chat, int, error) {
	userID, err := c.owner(actor)
	if err != nil {
		return nil, 0, err
	}
	return c.chats.ListChats(ctx, userID, includeArchived, limit, offset)
}

// Rename retitles a chat. Here the title is required: a rename to nothing is a mistake,
// where an unnamed new chat is a normal state.
func (c *Chats) Rename(ctx context.Context, chatID, title string, actor Actor) error {
	userID, err := c.owner(actor)
	if err != nil {
		return err
	}
	if strings.TrimSpace(chatID) == "" {
		return fmt.Errorf("%w: a chat id is required", ErrValidation)
	}

	clean, problem := checkTitle(title, true)
	if problem != "" {
		return titleProblem(problem)
	}
	if err := c.chats.RenameChat(ctx, userID, chatID, clean); err != nil {
		return mapAbsence(err)
	}
	return nil
}

// Archive puts a chat away.
//
// It is not a delete, and there is no delete. A chat's messages are the visible half of
// what an agent was asked to do and what it answered; the run transcripts that go with
// them are append-only, and a chat that could be deleted would leave those transcripts
// referring to a conversation nobody can read. Archiving twice is not an error — the
// caller wanted it gone, and it is gone.
func (c *Chats) Archive(ctx context.Context, chatID string, actor Actor) error {
	userID, err := c.owner(actor)
	if err != nil {
		return err
	}
	if strings.TrimSpace(chatID) == "" {
		return fmt.Errorf("%w: a chat id is required", ErrValidation)
	}

	if err := c.chats.ArchiveChat(ctx, userID, chatID, c.clock()); err != nil {
		return mapAbsence(err)
	}
	c.log.Info().Str("userId", userID).Str("chatId", chatID).Msg("chat archived")
	return nil
}

// Messages is what was said in one chat, oldest first, with the total.
//
// Tool and system rows are included as they are stored. Somebody reading their own
// conversation should be able to see that the agent went and looked something up, and
// hiding those rows here while showing them in a run transcript would be two accounts of
// one conversation.
func (c *Chats) Messages(ctx context.Context, chatID string, limit, offset int, actor Actor) ([]repository.Message, int, error) {
	userID, err := c.owner(actor)
	if err != nil {
		return nil, 0, err
	}
	if strings.TrimSpace(chatID) == "" {
		return nil, 0, fmt.Errorf("%w: a chat id is required", ErrValidation)
	}

	messages, total, err := c.messages.ListMessages(ctx, userID, chatID, limit, offset)
	if err != nil {
		return nil, 0, mapAbsence(err)
	}
	return messages, total, nil
}

// owner prepares the actor and returns the account its reads are scoped to.
//
// Every method here goes through it, so "a person's own chats and nobody else's" is one
// line rather than a rule repeated six times — and a machine key gets ErrForbidden from
// Owner() rather than an empty user id that would scope to nothing and look like an
// empty account.
func (c *Chats) owner(actor Actor) (string, error) {
	prepared, err := actor.prepare()
	if err != nil {
		return "", err
	}
	return prepared.Owner()
}

// checkTitle trims a title and reports what is wrong with it, or "" if nothing is.
func checkTitle(title string, required bool) (string, string) {
	clean := strings.TrimSpace(title)
	switch {
	case clean == "" && required:
		return "", "is required"
	case clean == "":
		return "New chat", ""
	case len([]rune(clean)) > maxChatTitleLength:
		return "", fmt.Sprintf("must be at most %d characters", maxChatTitleLength)
	}
	// A title is rendered in a list; a newline in one is a display problem rather than a
	// refusal, so it is flattened instead of rejected.
	return strings.Join(strings.Fields(clean), " "), ""
}

// titleProblem wraps a title complaint as a field error, so a client can put the message
// next to the input it belongs to.
func titleProblem(problem string) error {
	return fmt.Errorf("%w: title %s", ErrValidation, problem)
}

// mapAbsence turns the repository's not-found into this package's, and leaves everything
// else alone.
//
// A free function rather than a method because three services need it and none of them
// needs to vary it: a row that is absent, or present and somebody else's, is one answer.
func mapAbsence(err error) error {
	if errors.Is(err, repository.ErrNotFound) {
		return ErrNotFound
	}
	return err
}
