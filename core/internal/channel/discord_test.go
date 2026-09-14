package channel

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/bwmarrin/discordgo"
	"github.com/rs/zerolog"

	"github.com/ribdsp/wingman/core/internal/domain"
)

const discordTestToken = "test.discord.bot.token.value"

// discordSelf is the bot these tests are connected as, as Run would have filled it in.
var discordSelf = discordBot{id: "900900900"}

// discordMessage wraps a message the way the gateway delivers one.
func discordMessage(message *discordgo.Message) *discordgo.MessageCreate {
	return &discordgo.MessageCreate{Message: message}
}

func TestNewDiscord_refusesToBuildWithoutWhatItNeeds(t *testing.T) {
	// Arrange — an adapter with no handler would read the gateway and drop every message,
	// which looks exactly like a working connection to a quiet bot.
	cases := map[string]DiscordConfig{
		"no token":   {Handler: &fakeHandler{}},
		"no handler": {Token: discordTestToken},
	}

	for name, cfg := range cases {
		t.Run(name, func(t *testing.T) {
			// Act
			_, err := NewDiscord(cfg)

			// Assert
			if err == nil {
				t.Fatal("err = nil, want the adapter refused")
			}
			if cfg.Token != "" && strings.Contains(err.Error(), cfg.Token) {
				t.Errorf("err = %v, want it not to quote the token", err)
			}
		})
	}
}

func TestNewDiscord_asksForNoMoreThanItReads(t *testing.T) {
	// Arrange — an intent not asked for is data that never reaches this process at all. This
	// is the assertion that presence, membership and reactions stay off: three intents, and
	// message content among them because without it every message arrives with an empty body.

	// Act
	d, err := NewDiscord(DiscordConfig{Token: discordTestToken, Handler: &fakeHandler{}, Logger: testLogger()})

	// Assert
	if err != nil {
		t.Fatalf("NewDiscord: %v", err)
	}
	want := discordgo.IntentGuildMessages | discordgo.IntentDirectMessages | discordgo.IntentMessageContent
	if got := d.session.Identify.Intents; got != want {
		t.Errorf("intents = %d, want %d", got, want)
	}
	if d.Kind() != domain.ChannelDiscord {
		t.Errorf("Kind() = %s, want discord", d.Kind())
	}
}

func TestNewDiscord_handlesOneMessageAtATime(t *testing.T) {
	// Arrange — this SDK's default is a goroutine per event. Handling a message files work
	// and returns, and doing that one at a time means a flood waits on Discord's side rather
	// than becoming a hundred concurrent database calls. Heartbeats have their own goroutine,
	// so holding the read loop for the length of one message does not drop the connection.

	// Act
	d, err := NewDiscord(DiscordConfig{Token: discordTestToken, Handler: &fakeHandler{}, Logger: testLogger()})

	// Assert
	if err != nil {
		t.Fatalf("NewDiscord: %v", err)
	}
	if !d.session.SyncEvents {
		t.Error("syncEvents = false, want events handled one at a time")
	}
	// And nothing was asked of Discord yet, so who this bot is remains unknown until Run.
	if d.self != (discordBot{}) {
		t.Errorf("self = %+v, want it unknown until Run", d.self)
	}
}

func TestInboundFromDiscord_acceptsADirectMessage(t *testing.T) {
	// Arrange — no guild means a direct message, and a direct message needs no mention.
	event := discordMessage(&discordgo.Message{
		ID:        "M1",
		ChannelID: "DC0CONV",
		Content:   "what happened to revenue",
		Author:    &discordgo.User{ID: "42", Username: "ramap", GlobalName: "Rama Putra"},
	})

	// Act
	in, ok := inboundFromDiscord(event, discordSelf)

	// Assert
	if !ok {
		t.Fatal("ok = false, want the message accepted")
	}
	want := Inbound{
		Kind:           domain.ChannelDiscord,
		SenderID:       "42",
		SenderName:     "Rama Putra",
		ConversationID: "DC0CONV",
		Text:           "what happened to revenue",
		Direct:         true,
	}
	if in != want {
		t.Errorf("in = %+v, want %+v", in, want)
	}
}

func TestInboundFromDiscord_marksAMessageFromAnotherBot(t *testing.T) {
	// Arrange — two bots in one channel answering each other is a loop that spends real
	// money on both sides. The fact is recorded here; the decision belongs to the domain.
	event := discordMessage(&discordgo.Message{
		ChannelID: "DC0CONV",
		Content:   "revenue is up",
		Author:    &discordgo.User{ID: "999", Username: "other", Bot: true},
	})

	// Act
	in, ok := inboundFromDiscord(event, discordSelf)

	// Assert
	if !ok {
		t.Fatal("ok = false, want the message normalised so the domain can refuse it")
	}
	if !in.FromBot {
		t.Error("fromBot = false, want the platform's own answer carried through")
	}
}

func TestInboundFromDiscord_ignoresAnEventWithNothingToActOn(t *testing.T) {
	// Arrange — a webhook post and a system notice both arrive on this handler, and neither
	// has an account a chat identity could be linked to.
	cases := map[string]*discordgo.MessageCreate{
		"nothing at all":           nil,
		"an event with no message": {},
		"a message with no author": discordMessage(&discordgo.Message{
			ChannelID: "DC0CONV", Content: "what happened to revenue",
		}),
	}

	for name, event := range cases {
		t.Run(name, func(t *testing.T) {
			// Act
			_, ok := inboundFromDiscord(event, discordSelf)

			// Assert
			if ok {
				t.Error("ok = true, want the event ignored")
			}
		})
	}
}

func TestInboundFromDiscord_ignoresAMessageDiscordWroteRatherThanAPerson(t *testing.T) {
	// Arrange — a join notice, a pin and a boost all arrive as messages with an author. Each
	// would reach the model as an instruction that nobody typed.
	types := map[string]discordgo.MessageType{
		"a join notice":   discordgo.MessageTypeGuildMemberJoin,
		"a pinned notice": discordgo.MessageTypeChannelPinnedMessage,
		"a boost":         discordgo.MessageTypeUserPremiumGuildSubscription,
		"a thread start":  discordgo.MessageTypeThreadStarterMessage,
	}

	for name, messageType := range types {
		t.Run(name, func(t *testing.T) {
			// Act
			_, ok := inboundFromDiscord(discordMessage(&discordgo.Message{
				ChannelID: "DC0CONV", Content: "somebody joined", Type: messageType,
				Author: &discordgo.User{ID: "42", Username: "ramap"},
			}), discordSelf)

			// Assert
			if ok {
				t.Error("ok = true, want a message discord wrote ignored")
			}
		})
	}
}

func TestInboundFromDiscord_acceptsAReplyBecauseItIsSomebodyTyping(t *testing.T) {
	// Arrange — a reply is its own message type, and it is how a conversation in a thread
	// continues. Dropping it would make the bot answerable only in fresh messages.
	event := discordMessage(&discordgo.Message{
		ChannelID: "DC0CONV",
		Content:   "and last month?",
		Type:      discordgo.MessageTypeReply,
		Author:    &discordgo.User{ID: "42", Username: "ramap"},
	})

	// Act
	_, ok := inboundFromDiscord(event, discordSelf)

	// Assert
	if !ok {
		t.Error("ok = false, want a reply accepted")
	}
}

func TestInboundFromDiscord_inAServerForwardsOnlyAMessageAddressedToTheBot(t *testing.T) {
	// Arrange — a bot in a server room receives every message in it. Forwarding those would
	// mean a room's whole conversation reaching a model, on the account of whoever linked
	// first.
	author := &discordgo.User{ID: "42", Username: "ramap"}
	self := &discordgo.User{ID: discordSelf.id, Username: "wingman", Bot: true}
	cases := map[string]struct {
		message  *discordgo.Message
		want     string
		accepted bool
	}{
		"mentioned in the plain form": {
			message: &discordgo.Message{GuildID: "G1", ChannelID: "C1", Author: author,
				Content: "<@900900900> what happened to revenue", Mentions: []*discordgo.User{self}},
			want:     "what happened to revenue",
			accepted: true,
		},
		"mentioned in the nickname form": {
			message: &discordgo.Message{GuildID: "G1", ChannelID: "C1", Author: author,
				Content: "<@!900900900> what happened to revenue", Mentions: []*discordgo.User{self}},
			want:     "what happened to revenue",
			accepted: true,
		},
		"overheard": {
			message: &discordgo.Message{GuildID: "G1", ChannelID: "C1", Author: author,
				Content: "what happened to revenue"},
		},
		"somebody else was mentioned": {
			message: &discordgo.Message{GuildID: "G1", ChannelID: "C1", Author: author,
				Content:  "<@77> what happened to revenue",
				Mentions: []*discordgo.User{{ID: "77", Username: "someone"}}},
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			// Act
			in, ok := inboundFromDiscord(discordMessage(tc.message), discordSelf)

			// Assert
			if ok != tc.accepted {
				t.Fatalf("ok = %v, want %v", ok, tc.accepted)
			}
			if !tc.accepted {
				return
			}
			if in.Text != tc.want {
				t.Errorf("text = %q, want %q", in.Text, tc.want)
			}
			if in.Direct {
				t.Error("direct = true, want a server message marked as such")
			}
			if in.ConversationID != "C1" {
				t.Errorf("conversationId = %q, want the room the answer goes back to", in.ConversationID)
			}
		})
	}
}

func TestInboundFromDiscord_doesNotTreatAnEveryoneOrRolePingAsBeingSpokenTo(t *testing.T) {
	// Arrange — an @everyone reaches every member of the room by design, and a role ping
	// reaches everybody holding it. Reading either as an instruction would mean one person's
	// announcement starting a run that somebody else pays for.
	author := &discordgo.User{ID: "42", Username: "ramap"}
	cases := map[string]*discordgo.Message{
		"everyone": {GuildID: "G1", ChannelID: "C1", Author: author,
			Content: "@everyone the ledger is closed", MentionEveryone: true},
		"a role": {GuildID: "G1", ChannelID: "C1", Author: author,
			Content: "<@&5> the ledger is closed", MentionRoles: []string{"5"}},
	}

	for name, message := range cases {
		t.Run(name, func(t *testing.T) {
			// Act
			_, ok := inboundFromDiscord(discordMessage(message), discordSelf)

			// Assert
			if ok {
				t.Error("ok = true, want a broadcast left alone")
			}
		})
	}
}

func TestInboundFromDiscord_withNoIdentityYetForwardsNothingFromAServer(t *testing.T) {
	// Arrange — Run learns the bot's own id before it registers the handler, so this should
	// not happen. If it ever does, no mention can be matched, and the safe answer is to
	// forward nothing rather than to forward everything.
	event := discordMessage(&discordgo.Message{
		GuildID: "G1", ChannelID: "C1", Content: "<@900900900> what happened",
		Author:   &discordgo.User{ID: "42", Username: "ramap"},
		Mentions: []*discordgo.User{{ID: "900900900"}},
	})

	// Act
	_, ok := inboundFromDiscord(event, discordBot{})

	// Assert
	if ok {
		t.Error("ok = true, want nothing forwarded while the bot's own id is unknown")
	}
}

func TestInboundFromDiscord_fallsBackToTheUsernameWhenThereIsNoDisplayName(t *testing.T) {
	// Arrange — a person sees this in their own list of connected accounts, and Discord's
	// global name is optional.
	event := discordMessage(&discordgo.Message{
		ChannelID: "DC0CONV", Content: "what happened to revenue",
		Author: &discordgo.User{ID: "42", Username: "ramap"},
	})

	// Act
	in, ok := inboundFromDiscord(event, discordSelf)

	// Assert
	if !ok {
		t.Fatal("ok = false, want the message accepted")
	}
	if in.SenderName != "ramap" {
		t.Errorf("senderName = %q, want the username", in.SenderName)
	}
}

// discordAPI answers the adapter's HTTP request without anything leaving this process, and
// keeps what was sent.
//
// A transport rather than an httptest server, because discordgo builds its own URLs from
// package-level constants: replacing the client is the seam that does not involve writing to
// a global that other tests share.
type discordAPI struct {
	// reply is what Discord answers with, and status the code it answers with. The zero
	// values are a 200 carrying the least an object can carry.
	reply  string
	status int

	// path and body are what was asked for, and calls counts the requests — zero being how
	// a test proves a refusal happened before the network.
	path  string
	body  []byte
	calls int
}

func (a *discordAPI) RoundTrip(r *http.Request) (*http.Response, error) {
	a.calls++
	a.path = r.URL.Path
	if r.Body != nil {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			return nil, err
		}
		a.body = body
	}
	reply := a.reply
	if reply == "" {
		reply = `{"id":"1"}`
	}
	status := a.status
	if status == 0 {
		status = http.StatusOK
	}
	return &http.Response{
		StatusCode: status,
		Status:     http.StatusText(status),
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(reply)),
		Request:    r,
	}, nil
}

func TestDiscordSend_deliversTheTextAsWrittenAndMentionsNobody(t *testing.T) {
	// Arrange
	//
	// A model summarising an outage writes "@everyone" without meaning to notify anybody, and
	// on Discord that pings a whole server. The property is asserted on the wire rather than
	// on the struct because it rests on discordgo's own json tag: Parse is deliberately not
	// omitempty, so an empty list survives marshalling and means "resolve no mentions". A
	// version that added omitempty would silently start allowing every mention, and only a
	// test that reads the body would notice.
	api := &discordAPI{}
	adapter, err := NewDiscord(DiscordConfig{
		Token:   discordTestToken,
		Handler: &fakeHandler{},
		Logger:  zerolog.Nop(),
	})
	if err != nil {
		t.Fatalf("NewDiscord: %v", err)
	}
	adapter.session.Client = &http.Client{Transport: api}

	const answer = "@everyone revenue is down 12% and @here is the breakdown"

	// Act
	if err := adapter.Send(context.Background(), Reply{ConversationID: "DC0CONV", Text: answer}); err != nil {
		t.Fatalf("Send: %v", err)
	}

	// Assert
	var sent struct {
		Content         string `json:"content"`
		AllowedMentions *struct {
			Parse []string `json:"parse"`
		} `json:"allowed_mentions"`
	}
	if err := json.Unmarshal(api.body, &sent); err != nil {
		t.Fatalf("discord was sent something it could not parse: %v; body: %s", err, api.body)
	}
	// The words arrive exactly as the model wrote them. Stripping the @ would change an
	// answer a person is reading; refusing to resolve it does not.
	if sent.Content != answer {
		t.Errorf("content = %q, want the answer unchanged", sent.Content)
	}
	if sent.AllowedMentions == nil {
		t.Fatalf("allowed_mentions is absent, so discord resolves every mention it finds; body: %s", api.body)
	}
	if len(sent.AllowedMentions.Parse) != 0 {
		t.Errorf("allowed_mentions.parse = %v, want nothing resolvable", sent.AllowedMentions.Parse)
	}
}

func TestDiscordDirectTarget_opensADirectChannelBecauseAUserIdIsNotOne(t *testing.T) {
	// Arrange
	//
	// This is the one platform of the three where a link's stored id is not somewhere a
	// message can go: a user snowflake handed to ChannelMessageSendComplex is read as a
	// channel id and fails. So a DM has to be opened first, and what comes back is a
	// different id — which is exactly what this asserts, because a version that returned the
	// user id unchanged would look correct and never deliver anything.
	api := &discordAPI{reply: `{"id":"DM-CHANNEL-77","type":1}`}
	adapter, err := NewDiscord(DiscordConfig{
		Token:   discordTestToken,
		Handler: &fakeHandler{},
		Logger:  testLogger(),
	})
	if err != nil {
		t.Fatalf("NewDiscord: %v", err)
	}
	adapter.session.Client = &http.Client{Transport: api}

	// Act
	got, err := adapter.DirectTarget(context.Background(), "  4242424242  ")

	// Assert
	if err != nil {
		t.Fatalf("DirectTarget: %v", err)
	}
	if got != "DM-CHANNEL-77" {
		t.Errorf("conversation = %q, want the opened channel and not the user id", got)
	}
	if !strings.HasSuffix(api.path, "/users/@me/channels") {
		t.Errorf("path = %q, want the direct-channel endpoint", api.path)
	}
	var sent struct {
		RecipientID string `json:"recipient_id"`
	}
	if err := json.Unmarshal(api.body, &sent); err != nil {
		t.Fatalf("discord was sent something it could not parse: %v; body: %s", err, api.body)
	}
	if sent.RecipientID != "4242424242" {
		t.Errorf("recipient_id = %q, want the trimmed user id", sent.RecipientID)
	}
}

func TestDiscordDirectTarget_refusesARecipientThatIsNotThereWithoutCallingDiscord(t *testing.T) {
	// Arrange — an empty external id is a corrupted link, and a request for it would ask
	// Discord to open a conversation with nobody. Refused before the network, which is what
	// the call count proves.
	api := &discordAPI{}
	adapter, err := NewDiscord(DiscordConfig{
		Token:   discordTestToken,
		Handler: &fakeHandler{},
		Logger:  testLogger(),
	})
	if err != nil {
		t.Fatalf("NewDiscord: %v", err)
	}
	adapter.session.Client = &http.Client{Transport: api}

	// Act
	_, err = adapter.DirectTarget(context.Background(), "   ")

	// Assert
	if err == nil {
		t.Fatal("err = nil, want the recipient refused")
	}
	if api.calls != 0 {
		t.Errorf("calls = %d, want the refusal to happen before any request", api.calls)
	}
}

func TestDiscordDirectTarget_whenDiscordRefusesSaysSoWithoutTheToken(t *testing.T) {
	// Arrange — a person who has blocked the bot, or a snowflake that is no longer a user.
	// The notification is lost and that is acceptable; the token appearing in the log line
	// about it would not be.
	api := &discordAPI{status: http.StatusForbidden, reply: `{"message":"Cannot send messages to this user","code":50007}`}
	adapter, err := NewDiscord(DiscordConfig{
		Token:   discordTestToken,
		Handler: &fakeHandler{},
		Logger:  testLogger(),
	})
	if err != nil {
		t.Fatalf("NewDiscord: %v", err)
	}
	adapter.session.Client = &http.Client{Transport: api}

	// Act
	_, err = adapter.DirectTarget(context.Background(), "4242424242")

	// Assert
	if err == nil {
		t.Fatal("err = nil, want the failure reported")
	}
	if strings.Contains(err.Error(), discordTestToken) {
		t.Errorf("err = %v, want the bot token redacted", err)
	}
}
