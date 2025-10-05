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
	DialingChannel
	Connected
)

type txmode int

const (
	Normal txmode = iota
	Insert
)

type model struct {
	cmding bool
	cmdout *string
	error  *error
	prompt textinput.Model
	clm    *channellistmodel
	cm     *channelmodel
	gsd    *globalsettingsdata
}

type channellistmodel struct {
	channels []Channel
	list     list.Model
	gsd      *globalsettingsdata
}

type channelmodel struct {
	channel   Channel
	mode      txmode
	wsurl     string
	lrcconn   *websocket.Conn
	lexconn   *websocket.Conn
	cancel    func()
	vp        viewport.Model
	draft     textinput.Model
	msgs      map[uint32]*Message
	myid      *uint32
	render    []*string
	sentmsg   *string
	topic     *string
	signeturi *string
	datachan  chan []byte
	gsd       *globalsettingsdata
}

type globalsettingsdata struct {
	color  *uint32
	nick   *string
	handle *string
	xrpc   *PasswordClient
	width  int
	height int
	state  txstate
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
	prompt.Width = 28 //: + prompt.Width + 1 left over for blinky = initialWidth
	nick := "wanderer"
	color := uint32(33096)
	gsd := globalsettingsdata{
		nick:   &nick,
		color:  &color,
		width:  30,
		height: 20,
		state:  Splash,
	}
	return model{
		prompt: prompt,
		gsd:    &gsd,
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
			m.gsd.state = GettingChannels
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

func (cm *channelmodel) updateLRCIdentity() {
	if cm != nil && cm.lrcconn != nil {
		err := sendSet(cm.datachan, cm.gsd.nick, cm.gsd.handle, cm.gsd.color)
		if err != nil {
			send(errMsg{err})
		}
	}
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		if msg.String() == "ctrl+c" {
			return m, tea.Quit
		}
		if m.cmdout != nil {
			m.cmdout = nil
			return m, nil
		}
		if (m.cm != nil && m.cm.mode == Insert) || (m.clm != nil && m.clm.list.FilterState() == list.Filtering) {
			break
		}
		if !m.cmding {
			if msg.String() == ":" {
				m.cmding = true
				return m, m.prompt.Focus()
			}
		} else {
			switch msg.String() {
			case "esc":
				m.cmding = false
				m.prompt.Blur()
				m.prompt.SetValue("")
				return m, nil
			case "enter":
				m.cmding = false
				m.prompt.Blur()
				v := m.prompt.Value()
				m.prompt.SetValue("")
				return m, m.evaluateCommand(v)
			default:
				p, cmd := m.prompt.Update(msg)
				m.prompt = p
				return m, cmd
			}
		}
	case errMsg:
		m.gsd.state = Error
		m.error = &msg.err
		return m, nil
	case svMsg:
		if m.cm != nil && m.cm.myid != nil && msg.signetView.LrcId == *m.cm.myid {
			m.cm.signeturi = &msg.signetView.URI
			return m, nil
		}
	case dialMsg:
		if len(msg.value) == 1 {
			m.gsd.state = DialingChannel
			return m, m.dialingChannel(msg.value)
		}

	case loginMsg:
		if len(msg.value) == 2 {
			return m, login(msg.value[0], msg.value[1])
		}
	case loggedInMsg:
		m.gsd.xrpc = msg.xrpc
		return m, nil

	case setMsg:
		key, val, found := strings.Cut(msg.value, "=")
		if !found {
			return m, nil
		}
		switch key {
		case "color", "c":
			var b uint32

			if len(val) == 7 && val[0] == '#' {
				b64, err := strconv.ParseUint(val[1:], 16, 0)
				if err != nil {
					return m, nil
				}
				b = uint32(b64)
			} else {
				i, err := strconv.Atoi(val)
				if err != nil {
					return m, nil
				}
				b = uint32(i)
			}
			m.gsd.color = &b
			if m.cm != nil {
				m.cm.draft.PromptStyle = lipgloss.NewStyle().Foreground(ColorFromInt(&b))
			}
			m.cm.updateLRCIdentity()
			return m, nil
		case "nick", "name", "n":
			m.gsd.nick = &val
			if m.cm != nil {
				m.cm.draft.Prompt = renderName(m.gsd.nick, m.gsd.handle) + " "
				m.cm.draft.Width = m.gsd.width - len(m.cm.draft.Prompt) - 1
			}
			m.cm.updateLRCIdentity()
			return m, nil
		case "handle", "h", "at", "@":
			m.gsd.handle = &val
			if m.cm != nil {
				m.cm.draft.Prompt = renderName(m.gsd.nick, m.gsd.handle) + " "
				m.cm.draft.Width = m.gsd.width - len(m.cm.draft.Prompt) - 1
			}
			m.cm.updateLRCIdentity()
			return m, nil
		}

	case tea.WindowSizeMsg:
		m.gsd.height = msg.Height
		m.gsd.width = msg.Width
		m.prompt.Width = msg.Width - 2
		if m.clm != nil {
			m.clm.list.SetSize(msg.Width, msg.Height-1)
		}
		if m.cm != nil {
			m.cm.vp.Width = msg.Width
			m.cm.vp.Height = msg.Height - 2
			m.cm.draft.Width = m.gsd.width - len(m.cm.draft.Prompt) - 1
			if m.cm.render != nil {
				for _, message := range m.cm.msgs {
					message.renderMessage(msg.Width)
				}
				m.cm.vp.SetContent(JoinDeref(m.cm.render, ""))
			}
		}
		return m, nil
	}

	switch m.gsd.state {
	case Splash:
		return m.updateSplash(msg)
	case GettingChannels:
		return m.updateGettingChannels(msg)
	case ChannelList:
		clm, cmd, err := m.clm.updateChannelList(msg)
		if err != nil {
			m.gsd.state = Error
			m.error = &err
			return m, nil
		}
		m.clm = &clm
		return m, cmd
	case ResolvingChannel:
		return m.updateResolvingChannel(msg)
	case ConnectingToChannel:
		return m.updateConnectingToChannel(msg)
	case DialingChannel:

	case Connected:
		cm, cmd, err := m.cm.updateConnected(msg)
		if err != nil {
			m.gsd.state = Error
			m.error = &err
			return m, nil
		}
		m.cm = &cm
		return m, cmd
	}

	return m, nil
}

func (cm channelmodel) updateConnected(msg tea.Msg) (channelmodel, tea.Cmd, error) {
	switch msg := msg.(type) {
	case lrcEvent:
		if msg.e == nil {
			return cm, nil, errors.New("nil lrcEvent")
		}
		id := msg.e.Id
		switch msg := msg.e.Msg.(type) {
		case *lrcpb.Event_Ping:
			return cm, nil, nil
		case *lrcpb.Event_Pong:
			return cm, nil, nil
		case *lrcpb.Event_Init:
			err := initMessage(msg.Init, cm.msgs, &cm.render, cm.gsd.width)
			if err != nil {
				return cm, nil, err
			}
			if msg.Init.Echoed != nil && *msg.Init.Echoed {
				cm.myid = msg.Init.Id
			}
			ab := cm.vp.AtBottom()
			cm.vp.SetContent(JoinDeref(cm.render, ""))
			if ab {
				cm.vp.GotoBottom()
			}
			return cm, nil, nil
		case *lrcpb.Event_Pub:
			err := pubMessage(msg.Pub, cm.msgs, cm.gsd.width)
			if err != nil {
				return cm, nil, err
			}
			cm.vp.SetContent(JoinDeref(cm.render, ""))
			return cm, nil, err
		case *lrcpb.Event_Insert:
			err := insertMessage(msg.Insert, cm.msgs, &cm.render, cm.gsd.width)
			if err != nil {
				return cm, nil, err
			}
			ab := cm.vp.AtBottom()
			cm.vp.SetContent(JoinDeref(cm.render, ""))
			if ab {
				cm.vp.GotoBottom()
			}
			return cm, nil, nil
		case *lrcpb.Event_Delete:
			err := deleteMessage(msg.Delete, cm.msgs, &cm.render, cm.gsd.width)
			if err != nil {
				return cm, nil, err
			}
			ab := cm.vp.AtBottom()
			cm.vp.SetContent(JoinDeref(cm.render, ""))
			if ab {
				cm.vp.GotoBottom()
			}
			return cm, nil, nil
		case *lrcpb.Event_Mute:
			return cm, nil, nil
		case *lrcpb.Event_Unmute:
			return cm, nil, nil
		case *lrcpb.Event_Set:
			return cm, nil, nil
		case *lrcpb.Event_Get:
			if msg.Get.Topic != nil {
				cm.topic = msg.Get.Topic
			}
			return cm, nil, nil
		case *lrcpb.Event_Editbatch:
			if id == nil {
				return cm, nil, nil
			}
			err := editMessage(*id, msg.Editbatch.Edits, cm.msgs, &cm.render, cm.gsd.width)
			if err != nil {
				return cm, nil, err
			}
			ab := cm.vp.AtBottom()
			cm.vp.SetContent(JoinDeref(cm.render, ""))
			if ab {
				cm.vp.GotoBottom()
			}
			return cm, nil, nil
		}
	case tea.KeyMsg:
		switch cm.mode {
		case Normal:
			switch msg.String() {
			case "i", "a":
				cm.mode = Insert
				return cm, cm.draft.Focus(), nil
			case "I":
				cm.mode = Insert
				cm.draft.CursorStart()
				return cm, cm.draft.Focus(), nil
			case "A":
				cm.mode = Insert
				cm.draft.CursorEnd()
				return cm, cm.draft.Focus(), nil
			}
		case Insert:
			switch msg.String() {
			case "esc":
				cm.mode = Normal
				cm.draft.Blur()
				return cm, nil, nil
			case "enter":
				if cm.sentmsg != nil {
					if cm.gsd.xrpc != nil && cm.signeturi != nil {
						var color64 *uint64
						if cm.gsd.color != nil {
							c64 := uint64(*cm.gsd.color)
							color64 = &c64
						}
						lmr := lex.MessageRecord{
							SignetURI: *cm.signeturi,
							Body:      *cm.sentmsg,
							Nick:      cm.gsd.nick,
							Color:     color64,
							PostedAt:  syntax.DatetimeNow().String(),
						}
						cm.draft.SetValue("")
						cm.sentmsg = nil
						cm.myid = nil
						cm.signeturi = nil
						return cm, tea.Batch(sendPub(cm.lrcconn), createMSGCmd(cm.gsd.xrpc, &lmr)), nil
					}
					cm.draft.SetValue("")
					cm.sentmsg = nil
					return cm, sendPub(cm.lrcconn), nil
				}
				return cm, nil, nil
			}
		}
	}
	switch cm.mode {
	case Normal:
		vp, cmd := cm.vp.Update(msg)
		cm.vp = vp
		return cm, cmd, nil
	case Insert:
		draft, cmd := cm.draft.Update(msg)
		if cm.sentmsg == nil && draft.Value() != "" {
			nv := draft.Value()
			cm.sentmsg = &nv
			cm.draft = draft
			return cm, tea.Batch(cmd, sendInsert(cm.lrcconn, nv, 0, true)), nil
		}
		if cm.sentmsg != nil && *cm.sentmsg != draft.Value() {
			draftutf16 := utf16.Encode([]rune(draft.Value()))
			sentutf16 := utf16.Encode([]rune(*cm.sentmsg))
			edits := Diff(sentutf16, draftutf16)
			cm.draft = draft
			sentmsg := draft.Value()
			cm.sentmsg = &sentmsg
			return cm, tea.Batch(cmd, sendEditBatch(cm.datachan, edits)), nil
		}
		cm.draft = draft
		return cm, cmd, nil
	}
	return cm, nil, nil
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

func (m model) evaluateCommand(command string) tea.Cmd {
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
		case "dial":
			if len(parts) != 1 {
				return dialMsg{parts[1]}
			}
		}
		return nil
	}
}

type dialMsg struct {
	value string
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
		m.gsd.state = Connected
		cm := channelmodel{}
		cm.wsurl = msg.wsurl
		cm.gsd = m.gsd
		cm.cancel = msg.cancel
		cm.msgs = make(map[uint32]*Message)
		vp := viewport.New(m.gsd.width, m.gsd.height-2)
		cm.vp = vp
		draft := textinput.New()
		draft.Prompt = renderName(m.gsd.nick, m.gsd.handle) + " "
		draft.PromptStyle = lipgloss.NewStyle().Foreground(ColorFromInt(m.gsd.color))
		draft.Placeholder = "press i to start typing"
		draft.Width = m.gsd.width - len(draft.Prompt) - 1
		cm.draft = draft
		go startLRCHandlers(msg.conn, m.gsd.nick, m.gsd.handle, m.gsd.color)
		cm.lrcconn = msg.conn
		cm.lexconn = msg.lexconn
		cm.datachan = make(chan []byte)
		go listenToLexConn(msg.lexconn)
		go LRCWriter(cm.lrcconn, cm.datachan)
		m.cm = &cm
		m.clm = nil
		return m, nil
	}
	return m, nil
}

func (m model) updateDialingChannel(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case connSimpleMsg:
		m.gsd.state = Connected
		cm := channelmodel{}
		cm.gsd = m.gsd
		cm.cancel = msg.cancel
		cm.msgs = make(map[uint32]*Message)
		vp := viewport.New(m.gsd.width, m.gsd.height-2)
		cm.vp = vp
		draft := textinput.New()
		draft.Prompt = renderName(m.gsd.nick, m.gsd.handle) + " "
		draft.PromptStyle = lipgloss.NewStyle().Foreground(ColorFromInt(m.gsd.color))
		draft.Placeholder = "press i to start typing"
		draft.Width = m.gsd.width - len(draft.Prompt) - 1
		cm.draft = draft
		go startLRCHandlers(msg.conn, m.gsd.nick, m.gsd.handle, m.gsd.color)
		m.cm = &cm
		m.clm = nil
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

func startLRCHandlers(conn *websocket.Conn, nick *string, handle *string, color *uint32) {
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
		c := m.clm.curchannel()
		var host string
		if c != nil {
			host = c.Host
		}
		wsurl := fmt.Sprintf("%s%s", host, msg.resolution.URL)
		m.gsd.state = ConnectingToChannel
		ctx, cancel := context.WithCancel(context.Background())
		return m, m.connectToChannel(ctx, cancel, wsurl)
	}
	return m, nil
}

func (m model) dialingChannel(url string) tea.Cmd {
	return func() tea.Msg {
		dialer := websocket.DefaultDialer
		dialer.Subprotocols = []string{"lrc.v1"}
		ctx, cancel := context.WithCancel(context.Background())
		conn, _, err := dialer.DialContext(ctx, fmt.Sprintf("wss://%s", url), http.Header{})
		if err != nil {
			cancel()
			return errMsg{err}
		}
		return connSimpleMsg{conn, cancel}
	}
}

type connSimpleMsg struct {
	conn   *websocket.Conn
	cancel func()
}

func (m model) connectToChannel(ctx context.Context, cancel func(), wsurl string) tea.Cmd {
	return func() tea.Msg {
		dialer := websocket.DefaultDialer
		dialer.Subprotocols = []string{"lrc.v1"}
		conn, _, err := dialer.DialContext(ctx, fmt.Sprintf("wss://%s", wsurl), http.Header{})
		if err != nil {
			return errMsg{err}
		}

		dialer = websocket.DefaultDialer
		c := m.clm.curchannel()
		var uri string
		if c != nil {
			uri = c.URI
		}
		lexconn, _, err := dialer.DialContext(ctx, fmt.Sprintf("wss://xcvr.org/xrpc/org.xcvr.lrc.subscribeLexStream?uri=%s", uri), http.Header{})
		if err != nil {
			return errMsg{err}
		}
		return connMsg{conn, lexconn, cancel, wsurl}
	}
}

type connMsg struct {
	conn    *websocket.Conn
	lexconn *websocket.Conn
	cancel  func()
	wsurl   string
}

const (
	bullet   = "•"
	ellipsis = "…"
)

func (m model) updateGettingChannels(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case channelsMsg:
		clm := channellistmodel{}
		items := make([]list.Item, 0, len(msg.channels))
		for _, channel := range msg.channels {
			items = append(items, ChannelItem{channel})
		}
		list := list.New(items, ChannelItemDelegate{}, m.gsd.width, m.gsd.height-1)
		list.Styles = defaultStyles()
		list.Title = "org.xcvr.feed.getChannels"
		clm.list = list
		m.gsd.state = ChannelList
		clm.gsd = m.gsd
		m.clm = &clm
		return m, nil
	}
	return m, nil
}

func (clm channellistmodel) curchannel() *Channel {
	switch i := clm.list.SelectedItem().(type) {
	case ChannelItem:
		return &i.channel
	}
	return nil
}

func (clm channellistmodel) updateChannelList(msg tea.Msg) (channellistmodel, tea.Cmd, error) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "enter":
			if clm.list.FilterState() == list.Filtering {
				break
			}
			clm.gsd.state = ResolvingChannel
			cc := clm.curchannel()
			if cc != nil {
				uri := cc.URI
				did, _ := DidFromUri(uri)
				rkey, err := RkeyFromUri(uri)
				if err != nil {
					return clm, nil, err
				}
				return clm, ResolveChannel(cc.Host, did, rkey), nil
			} else {
				err := errors.New("bad list type")
				return clm, nil, err
			}
		}
	}
	list, cmd := clm.list.Update(msg)
	clm.list = list
	return clm, cmd, nil
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
	var pv string
	if m.cmding {
		pv = m.prompt.View()
	}
	switch m.gsd.state {
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
		return m.clm.channelListView(m.cmding, pv)
	case ResolvingChannel:
		return "resolving channel"
	case ConnectingToChannel:
		return m.connectingView()
	case Connected:
		return m.cm.connectedView(m.cmding, pv)
	default:
		return "under construction"
	}
}

func (cm channelmodel) connectedView(cmding bool, prompt string) string {
	vpt := cm.vp.View()
	var footer string
	if cmding {
		footer = prompt
	} else {
		address := "lrc://"
		address = fmt.Sprintf("%s%s", address, cm.wsurl)
		var topic string
		if cm.topic != nil {
			topic = *cm.topic
		}
		remainingspace := cm.gsd.width - len(address) - len(topic)
		var footertext string
		if remainingspace < 1 {
			addressremaining := cm.gsd.width - len(address)
			if addressremaining < 0 {
				footertext = strings.Repeat(" ", cm.gsd.width)
			} else {
				footertext = fmt.Sprintf("%s%s", address, strings.Repeat(" ", cm.gsd.width-len(address)))
			}
		} else {
			footertext = fmt.Sprintf("%s%s%s", address, strings.Repeat(" ", remainingspace), topic)
		}
		insert := cm.mode == Insert
		footerstyle := lipgloss.NewStyle().Reverse(insert)
		footerstyle = footerstyle.Foreground(ColorFromInt(cm.gsd.color))
		footer = footerstyle.Render(footertext)
	}
	draftText := cm.draft.View()
	return fmt.Sprintf("%s\n%s\n%s", vpt, draftText, footer)
}

func (m model) connectingView() string {
	return "resolving channel\nconnecting to channel"
}

func (clm channellistmodel) channelListView(cmding bool, prompt string) string {
	lv := clm.list.View()
	cv := ""
	if cmding {
		cv = prompt
	}
	return fmt.Sprintf("%s\n%s", lv, cv)
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
	offset := lipgloss.NewStyle().MarginLeft((m.gsd.width - 58) / 2)
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
