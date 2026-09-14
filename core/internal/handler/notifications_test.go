package handler

import (
	"errors"
	"net/http"
	"testing"

	"github.com/ribdsp/wingman/core/internal/repository"
)

// POST /v1/notifications, the one route the goal engine calls that is not a dispatch.
//
// What is asserted here is the boundary, not the fan-out: who may ask, what the envelope
// says when there is nobody to tell, and that a failed delivery does not return the
// platform's own error message. The recipient rule itself — the owner's live identities and
// nobody else's — is tested in internal/service/notifications_test.go, where the ladder is.

// triggerBody is the payload goal-engine/internal/core/client.go writes, field for field.
func triggerBody() notifyRequest {
	return notifyRequest{
		Kind:      "trigger",
		SubjectID: "goal-7",
		Headline:  "A goal fell behind and an agent was dispatched.",
		Link:      "https://wingman.example/goals/goal-7",
	}
}

func TestNotify_fromTheGoalEngineReachesTheOwnersChatAccount(t *testing.T) {
	// Arrange — the unattended owner has connected Telegram, which is the one-time step
	// deployment asks for. No recipient appears in the request.
	f := newFixture(t)
	f.channels.seed(f.unattendedOwner, repository.ChannelTelegram, "987654321", "Owner on Telegram")
	body := triggerBody()

	// Act
	rec := f.asBot(http.MethodPost, "/v1/notifications", body)

	// Assert
	env := decode(t, rec, http.StatusOK)
	var delivered deliveredView
	dataInto(t, env, &delivered)
	if delivered.Recipients != 1 {
		t.Errorf("recipients = %d, want 1", delivered.Recipients)
	}

	sent := f.sender.sent()
	if len(sent) != 1 {
		t.Fatalf("sends = %d, want 1", len(sent))
	}
	if sent[0].externalUserID != "987654321" {
		t.Errorf("recipient = %q, want the linked account's own id", sent[0].externalUserID)
	}
	if want := body.Headline + "\n" + body.Link; sent[0].text != want {
		t.Errorf("text = %q, want %q", sent[0].text, want)
	}

	// The answer is a count and nothing else. Which platforms somebody has connected is
	// their business, and the caller here holds a machine key.
	assertBodyOmits(t, rec, map[string]string{
		"bot key":               botSecret,
		"operator key":          operatorSecret,
		"recipient's chat id":   "987654321",
		"connected platform":    string(repository.ChannelTelegram),
		"unattended owner's id": f.unattendedOwner,
	})
}

func TestNotify_isAllowedForBothMachineRoles(t *testing.T) {
	// Arrange — an operator key is a machine key too, and an operator pinging their own
	// instance to see whether notifications work is a reasonable thing to do.
	for _, name := range []string{"operator", "bot"} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			f.channels.seed(f.unattendedOwner, repository.ChannelTelegram, "987654321", "Owner on Telegram")
			call := f.asBot
			if name == "operator" {
				call = f.asOperator
			}

			// Act
			rec := call(http.MethodPost, "/v1/notifications", triggerBody())

			// Assert
			var delivered deliveredView
			dataInto(t, decode(t, rec, http.StatusOK), &delivered)
			if delivered.Recipients != 1 {
				t.Errorf("recipients = %d, want 1", delivered.Recipients)
			}
		})
	}
}

func TestNotify_fromASignedInPersonIsForbidden(t *testing.T) {
	// Arrange — the same rule as dispatch, and for the same reason: a person must not be
	// able to make this instance message the operator's Telegram.
	f := newFixture(t)
	_, person := f.newAccount("person@wingman.test", "A person")
	f.channels.seed(f.unattendedOwner, repository.ChannelTelegram, "987654321", "Owner on Telegram")

	// Act
	rec := f.do(http.MethodPost, "/v1/notifications", person, triggerBody())

	// Assert
	assertErrorCode(t, rec, http.StatusForbidden, "FORBIDDEN")
	if sent := f.sender.sent(); len(sent) != 0 {
		t.Errorf("sends = %d, want none: refused before anything left the box", len(sent))
	}
}

func TestNotify_evenAboutTheirOwnAccountAPersonIsRefused(t *testing.T) {
	// Arrange — the person asking has a chat account of their own connected. The refusal is
	// about the caller, not about who would have received it: there is no recipient field to
	// point at themselves, and a route that messaged the owner on their behalf would be one.
	f := newFixture(t)
	person, token := f.newAccount("person@wingman.test", "A person")
	f.channels.seed(person.ID, repository.ChannelTelegram, "555111222", "A person on Telegram")

	// Act
	rec := f.do(http.MethodPost, "/v1/notifications", token, triggerBody())

	// Assert
	assertErrorCode(t, rec, http.StatusForbidden, "FORBIDDEN")
	if sent := f.sender.sent(); len(sent) != 0 {
		t.Errorf("sends = %d, want none", len(sent))
	}
}

func TestNotify_isRefusedWithoutACredential(t *testing.T) {
	// Arrange
	f := newFixture(t)

	// Act
	rec := f.unauthenticated(http.MethodPost, "/v1/notifications", triggerBody())

	// Assert
	assertErrorCode(t, rec, http.StatusUnauthorized, "UNAUTHORIZED")
}

func TestNotify_withNobodyLinkedIs200ThatSaysSo(t *testing.T) {
	// Arrange — notifications are on and the owner has connected nothing yet, which is every
	// instance until somebody performs the linking step.
	f := newFixture(t)

	// Act
	rec := f.asBot(http.MethodPost, "/v1/notifications", triggerBody())

	// Assert — a success, because a retry would find the same empty list. The message is what
	// carries the information, and the engine logs it.
	env := decode(t, rec, http.StatusOK)
	if !env.Success {
		t.Error("success = false, want true: nobody to tell is not a failed request")
	}
	var delivered deliveredView
	dataInto(t, env, &delivered)
	if delivered.Recipients != 0 {
		t.Errorf("recipients = %d, want 0", delivered.Recipients)
	}
	if env.Message != "Nobody was notified: no chat account is connected to the unattended owner." {
		t.Errorf("message = %q, want it to say plainly that nobody heard about it", env.Message)
	}
}

func TestNotify_aNoticeThatSaysNothingUsefulIs400(t *testing.T) {
	// Arrange
	cases := map[string]notifyRequest{
		"no kind":      {SubjectID: "goal-7", Headline: "A goal fell behind."},
		"unknown kind": {Kind: "escalation", SubjectID: "goal-7", Headline: "A goal fell behind."},
		"no subject":   {Kind: "trigger", Headline: "A goal fell behind."},
		"no headline":  {Kind: "trigger", SubjectID: "goal-7"},
		// A link is the one part of a notification somebody is invited to act on, so a
		// javascript: URL arriving with this instance's credibility attached to it is refused
		// at the boundary rather than sent and regretted.
		"a link that is not a web address": {
			Kind: "trigger", SubjectID: "goal-7", Headline: "A goal fell behind.",
			Link: "javascript:alert(1)",
		},
	}

	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			f.channels.seed(f.unattendedOwner, repository.ChannelTelegram, "987654321", "Owner on Telegram")

			// Act
			rec := f.asBot(http.MethodPost, "/v1/notifications", body)

			// Assert
			assertErrorCode(t, rec, http.StatusBadRequest, "VALIDATION_ERROR")
			if sent := f.sender.sent(); len(sent) != 0 {
				t.Errorf("sends = %d, want none", len(sent))
			}
		})
	}
}

func TestNotify_withABodyThatIsNotJSONIs400(t *testing.T) {
	// Arrange
	f := newFixture(t)

	// Act
	rec := f.do(http.MethodPost, "/v1/notifications", botSecret, "{not json")

	// Assert
	assertErrorCode(t, rec, http.StatusBadRequest, "VALIDATION_ERROR")
}

// unreachablePlatformToken is what a chat platform's own error looks like when a token is
// wrong: it carries the token. Nothing of it may reach a response body.
const unreachablePlatformToken = "1234567890:AAH-bot-token-that-must-not-be-echoed"

func TestNotify_whenEveryDeliveryFailsIs500WithoutThePlatformsMessage(t *testing.T) {
	// Arrange
	f := newFixture(t)
	f.channels.seed(f.unattendedOwner, repository.ChannelTelegram, "987654321", "Owner on Telegram")
	f.sender.err = errors.New("telegram: sendMessage: 401 Unauthorized for bot " + unreachablePlatformToken)

	// Act
	rec := f.asBot(http.MethodPost, "/v1/notifications", triggerBody())

	// Assert — a 500, because core could not do what it was asked. Not a sentinel, so the
	// detail is logged and the caller gets the request id instead.
	assertErrorCode(t, rec, http.StatusInternalServerError, "INTERNAL_ERROR")
	assertBodyOmits(t, rec, map[string]string{
		"chat platform's bot token": unreachablePlatformToken,
		"chat platform's message":   "401 Unauthorized",
	})
}

func TestNotify_hasNoHistoryToRead(t *testing.T) {
	// Arrange — a notification is a message that was sent, not a record. The goal engine's
	// audit log is what says a trigger fired and an approval is pending; a second list of the
	// same events, reachable with a bot key and no account scoping, would be a second answer
	// to the same question.
	f := newFixture(t)

	// Act + Assert
	assertErrorCode(t, f.asBot(http.MethodGet, "/v1/notifications", nil),
		http.StatusMethodNotAllowed, "NOT_FOUND")
}
