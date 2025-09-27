package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
	"unicode/utf16"

	"github.com/bluesky-social/indigo/api/atproto"
	"github.com/bluesky-social/indigo/atproto/client"
	"github.com/bluesky-social/indigo/atproto/identity"
	"github.com/bluesky-social/indigo/atproto/syntax"
	"github.com/bluesky-social/indigo/lex/util"
	"github.com/charmbracelet/bubbles/list"
	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/gorilla/websocket"
	"github.com/rachel-mp4/lrcproto/gen/go"
	"github.com/rachel-mp4/ttyxcvr/lex"
	"google.golang.org/protobuf/proto"
)

const White = lipgloss.Color("#ffffff")
const Black = lipgloss.Color("#000000")
const Olive = lipgloss.Color("#c6c013")
const Forest = lipgloss.Color("#034732")
const Green = lipgloss.Color("#008148")
const Orange = lipgloss.Color("#ef8a17")

var verySubduedColor = lipgloss.AdaptiveColor{Light: "#DDDADA", Dark: "#3C3C3C"}
var subduedColor = lipgloss.AdaptiveColor{Light: "#9B9B9B", Dark: "#5C5C5C"}
var subduedStyle = lipgloss.NewStyle().Foreground(subduedColor)

var PlainStyle = lipgloss.NewStyle().Foreground(White).Background(Black)

type txstate int

const (
	Splash txstate = iota
	Error
	GettingChannels
	ChannelList
	ResolvingChannel
	ConnectingToChannel
	Connected
)

type txmode int

const (
	Normal txmode = iota
	Command
	Insert
)

type model struct {
	state      txstate
	mode       txmode
	width      int
	height     int
	error      *error
	prompt     textinput.Model
	draft      *textinput.Model
	sentmsg    *string
	channels   *[]Channel
	list       *list.Model
	curchannel *Channel
	wsurl      *string
	lrcconn    *websocket.Conn
	lexconn    *websocket.Conn
	evtchan    chan []byte
	cancel     func()
	vp         *viewport.Model
	msgs       map[uint32]*Message
	myid       *uint32
	renders    []*string
	topic      *string
	color      *uint32
	nick       *string
	handle     *string
	signeturi  *string
	xrpc       *PasswordClient
}

type Message struct {
	nick     *string
	handle   *string
	color    *uint32
	active   bool
	text     string
	rendered *string
}

type Profile struct {
	Type        string  `json:"$type"`
	Did         string  `json:"did"`
	Handle      *string `json:"handle,omitempty"`
	DisplayName *string `json:"displayName,omitempty"`
	Status      *string `json:"status,omitempty"`
	Color       *uint32 `json:"color"`
}

type Channel struct {
	Type      string    `json:"$type"`
	URI       string    `json:"uri"`
	Host      string    `json:"host"`
	Creator   Profile   `json:"creator"`
	Title     string    `json:"title"`
	Topic     *string   `json:"topic,omitempty"`
	CreatedAt time.Time `json:"createdAt"`
}

type ChannelItem struct {
	channel Channel
}

func (c ChannelItem) Title() string {
	return c.channel.Title
}

func (c ChannelItem) Description() string {
	if c.channel.Topic != nil {
		return *c.channel.Topic
	}
	return ""
}

func (c ChannelItem) URI() string {
	return c.channel.URI
}
func (c ChannelItem) Host() string {
	return c.channel.Host
}

func (c ChannelItem) FilterValue() string {
	return c.channel.Title
}

type ChannelItemDelegate struct{}

func (d ChannelItemDelegate) Height() int                                  { return 3 }
func (d ChannelItemDelegate) Spacing() int                                 { return 0 }
func (d ChannelItemDelegate) Update(msg tea.Msg, list *list.Model) tea.Cmd { return nil }
func (d ChannelItemDelegate) Render(w io.Writer, m list.Model, index int, item list.Item) {
	var title string
	var desc string
	var uri string
	var host string
	var color *uint32
	var author string
	if i, ok := item.(ChannelItem); ok {
		title = i.Title()
		desc = i.Description()
		author = fmt.Sprintf("(%s)", renderName(i.channel.Creator.DisplayName, i.channel.Creator.Handle))
		host = subduedStyle.Render(fmt.Sprintf("(hosted on %s)", i.Host()))
		if desc == "" {
			desc = subduedStyle.Render("no provided description")
		}
		uri = i.URI()
		color = i.channel.Creator.Color
	} else {
		return
	}
	if index == m.Index() {
		greenStyle := lipgloss.NewStyle().Foreground(ColorFromInt(color))
		title = fmt.Sprintf("│%s %s", greenStyle.Render(title), author)
		desc = fmt.Sprintf("│%s", desc)
		uri = fmt.Sprintf("└%s", strings.Repeat("─", m.Width()-1))
	} else {
		s := lipgloss.NewStyle()
		s = s.Foreground(subduedColor)
		uri = s.Render(uri)
		host = subduedStyle.Render(author)
	}
	fmt.Fprintf(w, "%s %s\n%s\n%s", title, host, desc, uri)
}

func initialModel() model {
	prompt := textinput.New()
	prompt.Prompt = ":"
	nick := "wanderer"
	color := uint32(33096)
	return model{
		state:  Splash,
		mode:   Normal,
		prompt: prompt,
		width:  30,
		height: 20,
		nick:   &nick,
		color:  &color,
	}
}
func (m model) Init() tea.Cmd {
	return nil
}

func (m model) updateSplash(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "q":
			return m, tea.Quit
		default:
			m.state = GettingChannels
			return m, GetChannels
		}
	}
	return m, nil
}

func GetChannels() tea.Msg {
	c := &http.Client{Timeout: 10 * time.Second}
	res, err := c.Get("http://xcvr.org/xrpc/org.xcvr.feed.getChannels")

	if err != nil {
		return errMsg{err}
	}
	if res.StatusCode != 200 {
		return errMsg{errors.New(fmt.Sprintf("error getting channels: %d", res.StatusCode))}
	}
	decoder := json.NewDecoder(res.Body)
	var channels []Channel
	err = decoder.Decode(&channels)
	if err != nil {
		return errMsg{err}
	}
	return channelsMsg{channels}
}

type channelsMsg struct{ channels []Channel }

type errMsg struct{ err error }

func login(handle string, secret string) tea.Cmd {
	return func() tea.Msg {
		hdl, err := syntax.ParseHandle(handle)
		if err != nil {
			err = errors.New("handle failed to parse: " + err.Error())
			return errMsg{err}
		}
		id, err := identity.DefaultDirectory().LookupHandle(context.Background(), hdl)
		if err != nil {
			err = errors.New("handle failed to loopup: " + err.Error())
			return errMsg{err}
		}
		xrpc := NewPasswordClient(id.DID.String(), id.PDSEndpoint())
		err = xrpc.CreateSession(context.Background(), handle, secret)
		if err != nil {
			return errMsg{err}
		}
		return loggedInMsg{xrpc}
	}
}

type loggedInMsg struct {
	xrpc *PasswordClient
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case errMsg:
		m.state = Error
		m.error = &msg.err
		return m, nil
	case svMsg:
		if m.myid != nil && msg.signetView.LrcId == *m.myid {
			m.signeturi = &msg.signetView.URI
			return m, nil
		}

	case loginMsg:
		if len(msg.value) == 2 {
			return m, login(msg.value[0], msg.value[1])
		}
	case loggedInMsg:
		m.xrpc = msg.xrpc
		return m, nil

	case setMsg:
		key, val, found := strings.Cut(msg.value, "=")
		if !found {
			return m, nil
		}
		switch key {
		case "color", "c":
			i, err := strconv.Atoi(val)
			if err != nil {
				return m, nil
			}
			b := uint32(i)
			m.color = &b
			if m.draft != nil {
				m.draft.PromptStyle = lipgloss.NewStyle().Foreground(ColorFromInt(&b))
			}
			err = sendSet(m.evtchan, m.nick, m.handle, m.color)
			if err != nil {
				send(errMsg{err})
			}
			return m, nil
		case "nick", "name", "n":
			m.nick = &val
			if m.draft != nil {
				m.draft.Prompt = renderName(m.nick, m.handle) + " "
			}
			err := sendSet(m.evtchan, m.nick, m.handle, m.color)
			if err != nil {
				send(errMsg{err})
			}
			return m, nil
		case "handle", "h", "at", "@":
			m.handle = &val
			if m.draft != nil {
				m.draft.Prompt = renderName(m.nick, m.handle) + " "
			}
			err := sendSet(m.evtchan, m.nick, m.handle, m.color)
			if err != nil {
				send(errMsg{err})
			}
			return m, nil
		}

	case tea.WindowSizeMsg:
		m.height = msg.Height
		m.width = msg.Width
		if m.vp != nil {
			m.vp.Width = msg.Width
			m.vp.Height = msg.Height - 2
		}
		if m.renders != nil {
			for _, message := range m.msgs {
				message.renderMessage(msg.Width)
			}
			m.vp.SetContent(JoinDeref(m.renders, ""))
		}
		if m.list != nil {
			m.list.SetSize(msg.Width, msg.Height)
		}
		return m, nil

	case tea.KeyMsg:
		switch msg.String() {
		case "ctrl+c":
			return m, tea.Quit
		}
	}

	switch m.state {
	case Splash:
		return m.updateSplash(msg)
	case GettingChannels:
		return m.updateGettingChannels(msg)
	case ChannelList:
		return m.updateChannelList(msg)
	case ResolvingChannel:
		return m.updateResolvingChannel(msg)
	case ConnectingToChannel:
		return m.updateConnectingToChannel(msg)
	case Connected:
		return m.updateConnected(msg)
	}

	return m, nil
}

func (m model) updateConnected(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case lrcEvent:
		if msg.e == nil {
			m.state = Error
			err := errors.New("nil lrcEvent")
			m.error = &err
			return m, nil
		}
		id := msg.e.Id
		switch msg := msg.e.Msg.(type) {
		case *lrcpb.Event_Ping:
			return m, nil
		case *lrcpb.Event_Pong:
			return m, nil
		case *lrcpb.Event_Init:
			err := initMessage(msg.Init, m.msgs, &m.renders, m.width)
			if err != nil {
				m.state = Error
				m.error = &err
				return m, nil
			}
			if msg.Init.Echoed != nil && *msg.Init.Echoed {
				m.myid = msg.Init.Id
			}
			ab := m.vp.AtBottom()
			m.vp.SetContent(JoinDeref(m.renders, ""))
			if ab {
				m.vp.GotoBottom()
			}
			return m, nil
		case *lrcpb.Event_Pub:
			err := pubMessage(msg.Pub, m.msgs, m.width)
			if err != nil {
				m.state = Error
				m.error = &err
				return m, nil
			}
			m.vp.SetContent(JoinDeref(m.renders, ""))
			return m, nil
		case *lrcpb.Event_Insert:
			err := insertMessage(msg.Insert, m.msgs, &m.renders, m.width)
			if err != nil {
				m.state = Error
				m.error = &err
				return m, nil
			}
			ab := m.vp.AtBottom()
			m.vp.SetContent(JoinDeref(m.renders, ""))
			if ab {
				m.vp.GotoBottom()
			}
			return m, nil
		case *lrcpb.Event_Delete:
			err := deleteMessage(msg.Delete, m.msgs, &m.renders, m.width)
			if err != nil {
				m.state = Error
				m.error = &err
				return m, nil
			}
			ab := m.vp.AtBottom()
			m.vp.SetContent(JoinDeref(m.renders, ""))
			if ab {
				m.vp.GotoBottom()
			}
			return m, nil
		case *lrcpb.Event_Mute:
			return m, nil
		case *lrcpb.Event_Unmute:
			return m, nil
		case *lrcpb.Event_Set:
			return m, nil
		case *lrcpb.Event_Get:
			if msg.Get.Topic != nil {
				m.topic = msg.Get.Topic
			}
			return m, nil
		case *lrcpb.Event_Editbatch:
			if id == nil {
				return m, nil
			}
			err := editMessage(*id, msg.Editbatch.Edits, m.msgs, &m.renders, m.width)
			if err != nil {
				m.state = Error
				m.error = &err
				return m, nil
			}
			ab := m.vp.AtBottom()
			m.vp.SetContent(JoinDeref(m.renders, ""))
			if ab {
				m.vp.GotoBottom()
			}
			return m, nil
		}
	case tea.KeyMsg:
		switch m.mode {
		case Normal:
			switch msg.String() {
			case "i", "a":
				m.mode = Insert
				return m, m.draft.Focus()
			case "I":
				m.mode = Insert
				m.draft.CursorStart()
				return m, m.draft.Focus()
			case "A":
				m.mode = Insert
				m.draft.CursorEnd()
				return m, m.draft.Focus()
			case ":":
				m.mode = Command
				return m, m.prompt.Focus()
			}
		case Insert:
			switch msg.String() {
			case "esc":
				m.mode = Normal
				m.draft.Blur()
				return m, nil
			case "enter":
				if m.sentmsg != nil {
					if m.xrpc != nil && m.signeturi != nil {
						var color64 *uint64
						if m.color != nil {
							c64 := uint64(*m.color)
							color64 = &c64
						}
						lmr := lex.MessageRecord{
							SignetURI: *m.signeturi,
							Body:      *m.sentmsg,
							Nick:      m.nick,
							Color:     color64,
							PostedAt:  syntax.DatetimeNow().String(),
						}
						m.draft.SetValue("")
						m.sentmsg = nil
						m.myid = nil
						m.signeturi = nil
						return m, tea.Batch(sendPub(m.lrcconn), createMSGCmd(m.xrpc, &lmr))
					}
					m.draft.SetValue("")
					m.sentmsg = nil
					return m, sendPub(m.lrcconn)
				}
				return m, nil
			}
		case Command:
			switch msg.String() {
			case "esc":
				m.mode = Normal
				m.prompt.Blur()
				m.prompt.SetValue("")
				return m, nil
			case "enter":
				m.mode = Normal
				m.prompt.Blur()
				v := m.prompt.Value()
				m.prompt.SetValue("")
				return m, evaluateCommand(v)
			default:
			}
		}
	}
	switch m.mode {
	case Normal:
		vp, cmd := m.vp.Update(msg)
		m.vp = &vp
		return m, cmd
	case Command:
		prompt, cmd := m.prompt.Update(msg)
		m.prompt = prompt
		return m, cmd
	case Insert:
		draft, cmd := m.draft.Update(msg)
		if m.sentmsg == nil && draft.Value() != "" {
			nv := draft.Value()
			m.sentmsg = &nv
			m.draft = &draft
			return m, tea.Batch(cmd, sendInsert(m.lrcconn, nv, 0, true))
		}
		if m.sentmsg != nil && *m.sentmsg != draft.Value() {
			draftutf16 := utf16.Encode([]rune(draft.Value()))
			sentutf16 := utf16.Encode([]rune(*m.sentmsg))
			edits := Diff(sentutf16, draftutf16)
			m.draft = &draft
			sentmsg := draft.Value()
			m.sentmsg = &sentmsg
			return m, tea.Batch(cmd, sendEditBatch(m.evtchan, edits))
		}
		m.draft = &draft
		return m, cmd
	}
	return m, nil
}

func createMSGCmd(xrpc *PasswordClient, lmr *lex.MessageRecord) tea.Cmd {
	return func() tea.Msg {
		_, _, err := xrpc.CreateXCVRMessage(lmr, context.Background())
		if err != nil {
			return errMsg{err}
		}
		return nil
	}
}

func sendEditBatch(datachan chan []byte, edits []Edit) tea.Cmd {
	return func() tea.Msg {
		idx := 0
		batch := make([]*lrcpb.Edit, 0)
		for _, edit := range edits {
			switch edit.EditType {
			case EditDel:
				idx2 := idx + len(edit.Utf16Text)
				evt := makeDelete(uint32(idx), uint32(idx2))
				edit := lrcpb.Edit{Edit: &lrcpb.Edit_Delete{Delete: evt.GetDelete()}}
				batch = append(batch, &edit)
			case EditKeep:
				idx = idx + len(edit.Utf16Text)
			case EditAdd:
				evt := makeInsert(string(utf16.Decode(edit.Utf16Text)), uint32(idx))
				idx = idx + len(edit.Utf16Text)
				edit := lrcpb.Edit{Edit: &lrcpb.Edit_Insert{Insert: evt.GetInsert()}}
				batch = append(batch, &edit)
			}
		}
		evt := lrcpb.Event{Msg: &lrcpb.Event_Editbatch{Editbatch: &lrcpb.EditBatch{Edits: batch}}}
		data, err := proto.Marshal(&evt)
		if err != nil {
			return errMsg{err}
		}
		datachan <- data
		return nil
	}
}

func sendPub(conn *websocket.Conn) tea.Cmd {
	return func() tea.Msg {
		evt := &lrcpb.Event{Msg: &lrcpb.Event_Pub{Pub: &lrcpb.Pub{}}}
		data, err := proto.Marshal(evt)
		if err != nil {
			return errMsg{err}
		}
		err = conn.WriteMessage(websocket.BinaryMessage, data)
		if err != nil {
			return errMsg{err}
		}
		return nil
	}
}

func makeDelete(start uint32, end uint32) *lrcpb.Event {
	evt := &lrcpb.Event{Msg: &lrcpb.Event_Delete{Delete: &lrcpb.Delete{Utf16Start: start, Utf16End: end}}}
	return evt
}

func makeInsert(body string, idx uint32) *lrcpb.Event {
	evt := &lrcpb.Event{Msg: &lrcpb.Event_Insert{Insert: &lrcpb.Insert{Body: body, Utf16Index: idx}}}
	return evt
}

func sendInsert(conn *websocket.Conn, body string, utf16idx uint32, init bool) tea.Cmd {
	return func() tea.Msg {
		if init {
			evt := &lrcpb.Event{Msg: &lrcpb.Event_Init{Init: &lrcpb.Init{}}}
			data, err := proto.Marshal(evt)
			if err != nil {
				return errMsg{err}
			}
			if conn == nil {
				return nil
			}
			err = conn.WriteMessage(websocket.BinaryMessage, data)
			if err != nil {
				return errMsg{err}
			}
		}
		evt := &lrcpb.Event{Msg: &lrcpb.Event_Insert{Insert: &lrcpb.Insert{Body: body, Utf16Index: utf16idx}}}
		data, err := proto.Marshal(evt)
		if err != nil {
			return errMsg{err}
		}
		err = conn.WriteMessage(websocket.BinaryMessage, data)
		if err != nil {
			return errMsg{err}
		}
		return nil
	}
}

func evaluateCommand(command string) tea.Cmd {
	return func() tea.Msg {
		parts := strings.Split(command, " ")
		if parts == nil {
			return nil
		}
		switch parts[0] {
		case "q":
			return tea.QuitMsg{}
		case "se", "set":
			if len(parts) != 1 {
				return setMsg{parts[1]}
			}
		case "resize":
			return tea.WindowSize()
		case "login":
			if len(parts) != 1 {
				return loginMsg{parts[1:]}
			}
		}
		return nil
	}
}

type loginMsg struct {
	value []string
}

type setMsg struct {
	value string
}

// i think that the type of renders is a bit awkward, but i want deletemessage + friends to just modify the rendered
// messages slice in place in the event that we create a new msg. i think ideally the way to go is to make a more
// encapsulated data structure for the map + renders which still allows edits to the messages without requiring
// rerendering every message
func deleteMessage(msg *lrcpb.Delete, msgmap map[uint32]*Message, renders *[]*string, width int) error {
	if msg == nil {
		return errors.New("no insert")
	}
	id := msg.Id
	if id == nil {
		return errors.New("no insert id")
	}
	m := msgmap[*id]
	atr := false
	if m == nil {
		atr = true
		renderedDefault := ""
		m = &Message{
			nick:     nil,
			handle:   nil,
			color:    nil,
			active:   true,
			text:     "",
			rendered: &renderedDefault,
		}
		msgmap[*id] = m
	}
	start := msg.Utf16Start
	end := msg.Utf16End
	m.text = deleteBtwnUTF16Indices(m.text, start, end)
	m.renderMessage(width)
	if atr {
		*renders = append(*renders, m.rendered)
	}
	return nil
}
func deleteBtwnUTF16Indices(base string, start uint32, end uint32) string {
	if end <= start {
		return base
	}
	runes := []rune(base)
	baseUTF16Units := utf16.Encode(runes)
	if uint32(len(baseUTF16Units)) < end {
		end = uint32(len(baseUTF16Units))
	}
	if uint32(len(baseUTF16Units)) < start {
		return base
	}
	result := make([]uint16, 0, uint32(len(baseUTF16Units))+start-end)
	result = append(result, baseUTF16Units[:start]...)
	result = append(result, baseUTF16Units[end:]...)
	resultRunes := utf16.Decode(result)
	return string(resultRunes)
}

func editMessage(id uint32, edits []*lrcpb.Edit, msgmap map[uint32]*Message, renders *[]*string, width int) error {
	for _, edit := range edits {
		switch e := edit.Edit.(type) {
		case *lrcpb.Edit_Insert:
			ins := e.Insert
			ins.Id = &id
			err := insertMessage(ins, msgmap, renders, width)
			if err != nil {
				return err
			}
		case *lrcpb.Edit_Delete:
			del := e.Delete
			del.Id = &id
			err := deleteMessage(del, msgmap, renders, width)
			if err != nil {
				return err
			}
		}
	}
	return nil
}

func insertMessage(msg *lrcpb.Insert, msgmap map[uint32]*Message, renders *[]*string, width int) error {
	if msg == nil {
		return errors.New("no insert")
	}
	id := msg.Id
	if id == nil {
		return errors.New("no insert id")
	}
	m := msgmap[*id]
	atr := false
	if m == nil {
		atr = true
		renderedDefault := ""
		m = &Message{
			nick:     nil,
			handle:   nil,
			color:    nil,
			active:   true,
			text:     "",
			rendered: &renderedDefault,
		}
		msgmap[*id] = m
	}
	idx := msg.Utf16Index
	body := msg.Body
	m.text = insertAtUTF16Index(m.text, idx, body)

	m.renderMessage(width)
	if atr {
		*renders = append(*renders, m.rendered)
	}
	return nil
}

func insertAtUTF16Index(base string, index uint32, insert string) string {
	runes := []rune(base)
	baseUTF16Units := utf16.Encode(runes)
	if uint32(len(baseUTF16Units)) < index {
		spacesNeeded := index - uint32(len(baseUTF16Units))
		padding := strings.Repeat(" ", int(spacesNeeded))
		base = base + padding

		runes = []rune(base)
		baseUTF16Units = utf16.Encode(runes)
	}

	insertRunes := []rune(insert)
	insertUTF16Units := utf16.Encode(insertRunes)
	result := make([]uint16, 0, len(baseUTF16Units)+len(insertUTF16Units))
	result = append(result, baseUTF16Units[:index]...)
	result = append(result, insertUTF16Units...)
	result = append(result, baseUTF16Units[index:]...)
	resultRunes := utf16.Decode(result)
	return string(resultRunes)
}

func pubMessage(msg *lrcpb.Pub, msgmap map[uint32]*Message, width int) error {
	if msg == nil {
		return errors.New("no pub")
	}
	id := msg.Id
	if id == nil {
		return errors.New("no pub id")
	}
	m := msgmap[*id]
	if m != nil {
		m.active = false
		m.renderMessage(width)
	}
	return nil
}

func initMessage(msg *lrcpb.Init, msgmap map[uint32]*Message, renders *[]*string, width int) error {
	if msg == nil {
		return errors.New("beeped tf up")
	}
	id := msg.Id
	if id == nil {
		return errors.New("beeped up")
	}
	renderedDefault := ""
	m := &Message{
		nick:     msg.Nick,
		handle:   msg.ExternalID,
		color:    msg.Color,
		active:   true,
		text:     "",
		rendered: &renderedDefault,
	}
	m.renderMessage(width)
	msgmap[*id] = m
	*renders = append(*renders, m.rendered)
	return nil
}

func (m *Message) renderMessage(width int) {
	if m == nil {
		return
	}
	stylem := lipgloss.NewStyle().Width(width).Align(lipgloss.Left)
	styleh := stylem.Foreground(ColorFromInt(m.color))
	if m.active {
		styleh = styleh.Reverse(true)
		stylem = styleh
	}
	header := styleh.Render(renderName(m.nick, m.handle))
	body := stylem.Render(m.text)
	*m.rendered = fmt.Sprintf("%s\n%s\n", header, body)
}

func (m model) updateConnectingToChannel(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case connMsg:
		m.state = Connected
		m.cancel = msg.cancel
		m.msgs = make(map[uint32]*Message)
		vp := viewport.New(m.width, m.height-2)
		m.vp = &vp
		draft := textinput.New()
		draft.Prompt = renderName(m.nick, m.handle) + " "
		draft.PromptStyle = lipgloss.NewStyle().Foreground(ColorFromInt(m.color))
		draft.Placeholder = "press i to start typing"
		draft.Width = m.width
		m.draft = &draft
		go startLRCHandlers(msg.conn, msg.lexconn, m.nick, m.handle, m.color)
		m.lrcconn = msg.conn
		m.lexconn = msg.lexconn
		m.evtchan = make(chan []byte)
		go LRCWriter(m.lrcconn, m.evtchan)
		return m, nil
	}
	return m, nil
}

func LRCWriter(conn *websocket.Conn, datachan chan []byte) {
	for data := range datachan {
		err := conn.WriteMessage(websocket.BinaryMessage, data)
		if err != nil {
			send(errMsg{err})
			return
		}
	}
}

func renderName(nick *string, handle *string) string {
	var n string
	if nick != nil {
		n = *nick
	}
	var h string
	if handle != nil {
		h = fmt.Sprintf("@%s", *handle)
	}
	return fmt.Sprintf("%s%s", n, h)
}

func sendSet(datachan chan []byte, nick *string, handle *string, color *uint32) error {
	evt := &lrcpb.Event{Msg: &lrcpb.Event_Set{Set: &lrcpb.Set{Nick: nick, ExternalID: handle, Color: color}}}
	data, err := proto.Marshal(evt)
	if err != nil {
		return err
	}
	datachan <- data
	return nil

}

func startLRCHandlers(conn *websocket.Conn, lexconn *websocket.Conn, nick *string, handle *string, color *uint32) {
	if conn == nil {
		send(errMsg{errors.New("provided nil conn")})
		return
	}
	evt := &lrcpb.Event{Msg: &lrcpb.Event_Set{Set: &lrcpb.Set{Nick: nick, ExternalID: handle, Color: color}}}
	data, err := proto.Marshal(evt)
	if err != nil {
		send(errMsg{errors.New("failed to marshal: " + err.Error())})
		return
	}
	conn.WriteMessage(websocket.BinaryMessage, data)

	bep := "bep"
	evt = &lrcpb.Event{Msg: &lrcpb.Event_Get{Get: &lrcpb.Get{Topic: &bep}}}
	data, err = proto.Marshal(evt)
	if err != nil {
		send(errMsg{errors.New("failed to marshal: " + err.Error())})
		return
	}
	conn.WriteMessage(websocket.BinaryMessage, data)
	go listenToConn(conn)
	go listenToLexConn(lexconn)
}

type typedJSON struct {
	Type string `json:"$type"`
}

func listenToLexConn(conn *websocket.Conn) {
	for {
		var rawMsg json.RawMessage
		err := conn.ReadJSON(&rawMsg)
		if err != nil {
			send(errMsg{err})
			return
		}
		var typed typedJSON
		err = json.Unmarshal(rawMsg, &typed)
		if err != nil {
			send(errMsg{err})
			return
		}
		switch typed.Type {
		case "org.xcvr.lrc.defs#signetView":
			var sv SignetView
			err = json.Unmarshal(rawMsg, &sv)
			if err != nil {
				send(errMsg{err})
				return
			}
			send(svMsg{&sv})
		}
	}
}

type svMsg struct {
	signetView *SignetView
}

type SignetView struct {
	Type         string    `json:"$type,const=org.xcvr.lrc.defs#signetView"`
	URI          string    `json:"uri"`
	IssuerHandle string    `json:"issuerHandle"`
	ChannelURI   string    `json:"channelURI"`
	LrcId        uint32    `json:"lrcID"`
	AuthorHandle string    `json:"authorHandle"`
	StartedAt    time.Time `json:"startedAt"`
}

func listenToConn(conn *websocket.Conn) {
	for {
		_, data, err := conn.ReadMessage()
		if err != nil {
			send(errMsg{err})
		}
		var e lrcpb.Event
		err = proto.Unmarshal(data, &e)
		send(lrcEvent{&e})
	}
}

type lrcEvent struct{ e *lrcpb.Event }
type connlistenerexitMsg struct{}
type connwriterexitMsg struct{}

func (m model) updateResolvingChannel(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case resolutionMsg:
		wsurl := fmt.Sprintf("%s%s", m.curchannel.Host, msg.resolution.URL)
		m.wsurl = &wsurl
		m.state = ConnectingToChannel
		ctx, cancel := context.WithCancel(context.Background())
		return m, m.connectToChannel(ctx, cancel)
	}
	return m, nil
}

func (m model) connectToChannel(ctx context.Context, cancel func()) tea.Cmd {
	return func() tea.Msg {
		dialer := websocket.DefaultDialer
		dialer.Subprotocols = []string{"lrc.v1"}
		if m.wsurl == nil {
			return errMsg{errors.New("nil wsurl!")}
		}
		conn, _, err := dialer.DialContext(ctx, fmt.Sprintf("wss://%s", *m.wsurl), http.Header{})
		if err != nil {
			return errMsg{err}
		}

		dialer = websocket.DefaultDialer
		lexconn, _, err := dialer.DialContext(ctx, fmt.Sprintf("wss://xcvr.org/xrpc/org.xcvr.lrc.subscribeLexStream?uri=%s", m.curchannel.URI), http.Header{})
		if err != nil {
			return errMsg{err}
		}
		return connMsg{conn, lexconn, cancel}
	}
}

type connMsg struct {
	conn    *websocket.Conn
	lexconn *websocket.Conn
	cancel  func()
}

const (
	bullet   = "•"
	ellipsis = "…"
)

func (m model) updateGettingChannels(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case channelsMsg:
		items := make([]list.Item, 0, len(msg.channels))
		for _, channel := range msg.channels {
			items = append(items, ChannelItem{channel})
		}
		list := list.New(items, ChannelItemDelegate{}, m.width, m.height)
		list.Styles = defaultStyles()
		list.Title = "org.xcvr.feed.getChannels"
		m.list = &list
		m.state = ChannelList
		return m, nil
	}
	return m, nil
}

func (m model) updateChannelList(msg tea.Msg) (tea.Model, tea.Cmd) {
	if m.list == nil {
		err := errors.New("no list!")
		m.error = &err
		m.state = Error
		return m, nil
	}
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "enter":
			m.state = ResolvingChannel
			i, ok := m.list.SelectedItem().(ChannelItem)
			if ok {
				uri := i.URI()
				did, _ := DidFromUri(uri)
				rkey, err := RkeyFromUri(uri)
				if err != nil {
					m.error = &err
					m.state = Error
					return m, nil
				}
				m.curchannel = &i.channel
				m.list = nil
				m.channels = nil
				return m, ResolveChannel(i.Host(), did, rkey)
			} else {
				err := errors.New("bad list type")
				m.error = &err
				m.state = Error
				return m, nil
			}
		}
	}
	list, cmd := m.list.Update(msg)
	m.list = &list
	return m, cmd
}

func ResolveChannel(host string, did string, rkey string) tea.Cmd {
	return func() tea.Msg {
		c := &http.Client{Timeout: 10 * time.Second}
		res, err := c.Get(fmt.Sprintf("http://%s/xrpc/org.xcvr.actor.resolveChannel?did=%s&rkey=%s", host, did, rkey))

		if err != nil {
			return errMsg{err}
		}
		if res.StatusCode != 200 {
			return errMsg{errors.New(fmt.Sprintf("error resolving channel: %d", res.StatusCode))}
		}
		decoder := json.NewDecoder(res.Body)
		var resolution Resolution
		err = decoder.Decode(&resolution)
		if err != nil {
			return errMsg{err}
		}
		return resolutionMsg{resolution}
	}
}

type resolutionMsg struct {
	resolution Resolution
}

type Resolution struct {
	URL string  `json:"url"`
	URI *string `json:"uri,omitempty"`
}

func (m model) View() string {
	switch m.state {
	case Splash:
		return m.splashView()
	case GettingChannels:
		return "loading..."
	case Error:
		if m.error != nil {
			return (*m.error).Error()
		}
		return "broke so bad there isn't an error"
	case ChannelList:
		return m.channelListView()
	case ResolvingChannel:
		return "resolving channel"
	case ConnectingToChannel:
		return m.connectingView()
	case Connected:
		return m.connectedView()
	default:
		return "under construction"
	}
}

func (m model) connectedView() string {
	var vpt string
	if m.vp != nil {
		vpt = m.vp.View()
	}
	address := "lrc://"
	if m.wsurl != nil {
		address = fmt.Sprintf("%s%s", address, *m.wsurl)
	}
	var topic string
	if m.topic != nil {
		topic = *m.topic
	}
	remainingspace := m.width - len(address) - len(topic)
	var footertext string
	if m.mode == Command {
		footertext = m.prompt.View()
	} else if remainingspace < 1 {
		footertext = fmt.Sprintf("%s%s", address, strings.Repeat(" ", m.width-len(address)))
	} else {
		footertext = fmt.Sprintf("%s%s%s", address, strings.Repeat(" ", remainingspace), topic)
	}
	insert := m.mode == Insert
	footerstyle := lipgloss.NewStyle().Reverse(insert)
	if m.mode != Command {
		footerstyle = footerstyle.Foreground(ColorFromInt(m.color))
	}
	footer := footerstyle.Render(footertext)
	var draftText string
	if m.draft != nil {
		draftText = m.draft.View()
	}
	return fmt.Sprintf("%s\n%s\n%s", vpt, draftText, footer)
}

func (m model) connectingView() string {
	blip := m.wsurl
	if blip == nil {
		return "resolving channel\nSOMETHING WENT HORRIBLY WRONG"
	}
	return fmt.Sprintf("resolving channel\nconnecting to %s", *m.wsurl)
}

func (m model) channelListView() string {
	return m.list.View()
}

func (m model) splashView() string {
	style := lipgloss.NewStyle().Foreground(Green)
	part00 := "\n              ⣰⡀ ⢀⣀ ⡇ ⡇⡠   ⣰⡀ ⢀⡀   ⡀⢀ ⢀⡀ ⡀⢀ ⡇"
	part01 := "\n              ⠘⠤ ⠣⠼ ⠣ ⠏⠢   ⠘⠤ ⠣⠜   ⣑⡺ ⠣⠜ ⠣⠼ ⠅"
	part02 := "\n              ⣰⡀ ⡀⣀ ⢀⣀ ⣀⡀ ⢀⣀ ⢀⣀ ⢀⡀ ⠄ ⡀⢀ ⢀⡀ ⡀⣀"
	part03 := "\n              ⠘⠤ ⠏  ⠣⠼ ⠇⠸ ⠭⠕ ⠣⠤ ⠣⠭ ⠇ ⠱⠃ ⠣⠭ ⠏"
	part1 := "\n\n  %%%%%%%          "
	text1 := "tty!xcvr\n"
	part2 := ` %%%%%%%%%%  %%%            %   %           %%%%%%%%
   %%%%%%%%%%%%%%%%%        %%%%     %%%%%%%%%%%%%%%%%%
     %%%%%%%%%%%%%%%%%%%%%%% %% %%%%%%%%%%%%%%%%%%%%%%%%%%
       %%%%%%%%%%%%%%%%%%%%% %% %%%%%%%%%%%%%%%%%%%%%%%
            %%%%%%%%%%%%%%%% %% %%%%%%%%%%%%%%%%%%%%%
         %%%%%%%%%%%%%%%%%%% %% %%%%%%%%%%%%%%
       %%%%%%%%%%%%%%%%%%%%%    %%%%%%%%%%%%%%%%%%
          %%%%%%%%%%%%         %%%%%%%%%%%%%%%%`
	part25 := "\n             %%%%%   "
	text2 := `made with`
	part3 := "   %%%%%%%%%\n"
	text3 := "    love by moth11."

	part4 := "      %\n\n"

	text4 := `  talk to you! 
          transceiver
                           press a key
                                to start!
  `
	s := fmt.Sprintf("\n\n\n\n%s%s%s%s%s%s%s%s%s%s%s%s%s", style.Render(part00), style.Render(part01), style.Render(part02), style.Render(part03), style.Render(part1), text1, style.Render(part2), style.Render(part25), text2, style.Render(part3), text3, style.Render(part4), text4)
	offset := lipgloss.NewStyle().MarginLeft((m.width - 58) / 2)
	return offset.Render(s)
}

var send func(msg tea.Msg)

func main() {
	fmt.Println("if you can see me before program quits i think that you should find a better terminal,")
	p := tea.NewProgram(initialModel(), tea.WithAltScreen())
	send = p.Send
	if _, err := p.Run(); err != nil {
		fmt.Printf("Alas, there's been an error: %v", err)
		os.Exit(1)
	}
	fmt.Println("kthxbai!")
}

func DidFromUri(uri string) (did string, err error) {
	s, err := trimScheme(uri)
	if err != nil {
		return
	}
	ss, err := uriFragSplit(s)
	if err != nil {
		return
	}
	did = ss[0]
	return
}

func trimScheme(uri string) (string, error) {
	s, ok := strings.CutPrefix(uri, "at://")
	if !ok {
		return "", errors.New("not a uri, missing at:// scheme")
	}
	return s, nil
}

func uriFragSplit(urifrag string) ([]string, error) {
	ss := strings.Split(urifrag, "/")
	if len(ss) != 3 {
		return nil, errors.New("not a urifrag, incorrect number of bits")
	}
	return ss, nil
}

func RkeyFromUri(uri string) (rkey string, err error) {
	s, err := trimScheme(uri)
	if err != nil {
		return
	}
	ss, err := uriFragSplit(s)
	if err != nil {
		return
	}
	rkey = ss[2]
	return
}

func defaultStyles() (s list.Styles) {
	s.TitleBar = lipgloss.NewStyle().Padding(0, 0, 0, 0) //nolint:mnd

	s.Title = lipgloss.NewStyle().
		Foreground(subduedColor).
		Padding(0, 0)

	s.Spinner = lipgloss.NewStyle().
		Foreground(lipgloss.AdaptiveColor{Light: "#8E8E8E", Dark: "#747373"})

	s.FilterPrompt = lipgloss.NewStyle().
		Foreground(lipgloss.AdaptiveColor{Light: "#04B575", Dark: "#ECFD65"})

	s.FilterCursor = lipgloss.NewStyle().
		Foreground(lipgloss.AdaptiveColor{Light: "#EE6FF8", Dark: "#EE6FF8"})

	s.DefaultFilterCharacterMatch = lipgloss.NewStyle().Underline(true)

	s.StatusBar = lipgloss.NewStyle().
		Foreground(lipgloss.AdaptiveColor{Light: "#A49FA5", Dark: "#777777"}).
		Padding(0, 0, 0, 0) //nolint:mnd

	s.StatusEmpty = lipgloss.NewStyle().Foreground(subduedColor)

	s.StatusBarActiveFilter = lipgloss.NewStyle().
		Foreground(lipgloss.AdaptiveColor{Light: "#1a1a1a", Dark: "#dddddd"})

	s.StatusBarFilterCount = lipgloss.NewStyle().Foreground(verySubduedColor)

	s.NoItems = lipgloss.NewStyle().
		Foreground(lipgloss.AdaptiveColor{Light: "#909090", Dark: "#626262"})

	s.ArabicPagination = lipgloss.NewStyle().Foreground(subduedColor)

	s.PaginationStyle = lipgloss.NewStyle().PaddingLeft(2) //nolint:mnd

	s.HelpStyle = lipgloss.NewStyle().Padding(1, 0, 0, 2) //nolint:mnd

	s.ActivePaginationDot = lipgloss.NewStyle().
		Foreground(lipgloss.AdaptiveColor{Light: "#847A85", Dark: "#979797"}).
		SetString(bullet)

	s.InactivePaginationDot = lipgloss.NewStyle().
		Foreground(verySubduedColor).
		SetString(bullet)

	s.DividerDot = lipgloss.NewStyle().
		Foreground(verySubduedColor).
		SetString(" " + bullet + " ")

	return s
}

const maxInt = int(^uint(0) >> 1)

func JoinDeref(elems []*string, sep string) string {
	switch len(elems) {
	case 0:
		return ""
	case 1:
		return *elems[0]
	}

	var n int
	if len(sep) > 0 {
		if len(sep) >= maxInt/(len(elems)-1) {
			panic("strings: Join output length overflow")
		}
		n += len(sep) * (len(elems) - 1)
	}
	for _, elem := range elems {
		if len(*elem) > maxInt-n {
			panic("strings: Join output length overflow")
		}
		n += len(*elem)
	}

	var b strings.Builder
	b.Grow(n)
	b.WriteString(*elems[0])
	for _, s := range elems[1:] {
		b.WriteString(sep)
		b.WriteString(*s)
	}
	return b.String()
}

type PasswordClient struct {
	xrpc       *client.APIClient
	accessjwt  *string
	refreshjwt *string
	did        *string
}

func NewPasswordClient(did string, host string) *PasswordClient {
	return &PasswordClient{
		xrpc: client.NewAPIClient(host),
		did:  &did,
	}
}

func (c *PasswordClient) CreateSession(ctx context.Context, identity string, secret string) error {
	input := atproto.ServerCreateSession_Input{
		Identifier: identity,
		Password:   secret,
	}
	var out atproto.ServerCreateSession_Output
	err := c.xrpc.LexDo(ctx, "POST", "application/json", "com.atproto.server.createSession", nil, input, &out)
	if err != nil {
		return errors.New("I couldn't create a session: " + err.Error())
	}
	c.accessjwt = &out.AccessJwt
	c.refreshjwt = &out.RefreshJwt
	return nil
}

func (c *PasswordClient) RefreshSession(ctx context.Context) error {
	c.xrpc.Headers.Set("Authorization", fmt.Sprintf("Bearer %s", *c.refreshjwt))
	var out atproto.ServerRefreshSession_Output
	err := c.xrpc.LexDo(ctx, "POST", "application/json", "com.atproto.server.refreshSession", nil, nil, &out)
	if err != nil {
		return errors.New("failed to refresh session! " + err.Error())
	}
	c.accessjwt = &out.AccessJwt
	c.refreshjwt = &out.RefreshJwt
	return nil
}

func (c *PasswordClient) CreateXCVRMessage(message *lex.MessageRecord, ctx context.Context) (cid string, uri string, err error) {
	input := atproto.RepoCreateRecord_Input{
		Collection: "org.xcvr.lrc.message",
		Repo:       *c.did,
		Record:     &util.LexiconTypeDecoder{Val: message},
	}
	return c.createMyRecord(input, ctx)
}

func (c *PasswordClient) createMyRecord(input atproto.RepoCreateRecord_Input, ctx context.Context) (cid string, uri string, err error) {
	if c.accessjwt == nil {
		err = errors.New("must create a session first")
		return
	}
	c.xrpc.Headers.Set("Authorization", fmt.Sprintf("Bearer %s", *c.accessjwt))
	var out atproto.RepoCreateRecord_Output
	err = c.xrpc.LexDo(ctx, "POST", "application/json", "com.atproto.repo.createRecord", nil, input, &out)
	if err != nil {
		err1 := err.Error()
		err = c.RefreshSession(ctx)
		if err != nil {
			err = errors.New(fmt.Sprintf("failed to refresh session while creating %s! first %s then %s", input.Collection, err1, err.Error()))
			return
		}
		c.xrpc.Headers.Set("Authorization", fmt.Sprintf("Bearer %s", *c.accessjwt))
		out = atproto.RepoCreateRecord_Output{}
		err = c.xrpc.LexDo(ctx, "POST", "application/json", "com.atproto.repo.createRecord", nil, input, &out)
		if err != nil {
			err = errors.New(fmt.Sprintf("not good, failed to create %s after failing then refreshing session! first %s then %s", input.Collection, err1, err.Error()))
			return
		}
		cid = out.Cid
		uri = out.Uri
		return
	}
	cid = out.Cid
	uri = out.Uri
	return
}

func ColorFromInt(c *uint32) lipgloss.Color {
	if c == nil {
		return Green
	}
	ui := *c
	guess := fmt.Sprintf("#%06x", ui)
	return lipgloss.Color(guess[0:7])
}
