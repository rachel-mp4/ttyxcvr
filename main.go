package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
	"unicode/utf16"

	"github.com/charmbracelet/bubbles/list"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/gorilla/websocket"
	"github.com/rachel-mp4/lrcproto/gen/go"
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
	command    *string
	channels   *[]Channel
	list       *list.Model
	curchannel *Channel
	wsurl      *string
	lrcconn    *websocket.Conn
	cancel     func()
	vp         *viewport.Model
	msgs       map[uint32]*Message
	renders    []*string
	topic      *string
	color      *uint32
	nick       *string
	handle     *string
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
	if i, ok := item.(ChannelItem); ok {
		title = i.Title()
		desc = i.Description()
		host = subduedStyle.Render(fmt.Sprintf("(hosted by %s)", i.Host()))
		if desc == "" {
			desc = subduedStyle.Render("no provided description")
		}
		uri = i.URI()
	} else {
		return
	}
	if index == m.Index() {
		greenStyle := lipgloss.NewStyle().Foreground(Green)
		title = fmt.Sprintf("│%s %s", greenStyle.Render(title), host)
		desc = fmt.Sprintf("│%s", desc)
		uri = fmt.Sprintf("└%s", strings.Repeat("─", m.Width()-1))
	} else {
		s := lipgloss.NewStyle()
		s = s.Foreground(subduedColor)
		uri = s.Render(uri)
	}
	fmt.Fprintf(w, "%s %s\n%s\n%s", title, host, desc, uri)
}

func initialModel() model {
	return model{
		state:  Splash,
		mode:   Normal,
		width:  30,
		height: 20,
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

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case errMsg:
		m.state = Error
		m.error = &msg.err

	case tea.WindowSizeMsg:
		m.height = msg.Height
		m.width = msg.Width

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
			m.vp.SetContent(JoinDeref(m.renders, ""))
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
			m.vp.SetContent(JoinDeref(m.renders, ""))
			return m, nil
		case *lrcpb.Event_Delete:
			err := deleteMessage(msg.Delete, m.msgs, &m.renders, m.width)
			if err != nil {
				m.state = Error
				m.error = &err
				return m, nil
			}
			m.vp.SetContent(JoinDeref(m.renders, ""))
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
			return m, nil

		}
	}
	vp, cmd := m.vp.Update(msg)
	m.vp = &vp
	return m, cmd
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
	styleh := stylem.Foreground(Green)
	if m.active {
		styleh = styleh.Reverse(true)
		stylem = styleh
	}
	var nick string
	if m.nick != nil {
		nick = *m.nick
	}
	var handle string
	if m.handle != nil && *m.handle != "" {
		handle = fmt.Sprintf("@%s", *m.handle)
	}
	header := styleh.Render(fmt.Sprintf("%s%s", nick, handle))
	body := stylem.Render(m.text)
	*m.rendered = fmt.Sprintf("%s\n%s\n", header, body)
}

func (m model) updateConnectingToChannel(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case connMsg:
		m.state = Connected
		m.cancel = msg.cancel
		m.msgs = make(map[uint32]*Message)
		vp := viewport.New(m.width, m.height-1)
		m.vp = &vp
		go startLRCHandlers(msg.conn)
		return m, nil
	}
	return m, nil
}

func startLRCHandlers(conn *websocket.Conn) {
	if conn == nil {
		send(errMsg{errors.New("provided nil conn")})
		return
	}
	nick := "wanderer"
	evt := &lrcpb.Event{Msg: &lrcpb.Event_Set{Set: &lrcpb.Set{Nick: &nick}}}
	data, err := proto.Marshal(evt)
	if err != nil {
		send(errMsg{errors.New("failed to marshal: " + err.Error())})
		return
	}

	evt = &lrcpb.Event{Msg: &lrcpb.Event_Get{Get: &lrcpb.Get{Topic: &nick}}}
	data, err = proto.Marshal(evt)
	if err != nil {
		send(errMsg{errors.New("failed to marshal: " + err.Error())})
		return
	}
	conn.WriteMessage(websocket.BinaryMessage, data)
	go listenToConn(conn)
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
		return connMsg{conn, cancel}
	}
}

type connMsg struct {
	conn   *websocket.Conn
	cancel func()
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
	if remainingspace < 1 {
		footertext = address
	} else {
		footertext = fmt.Sprintf("%s%s%s", address, strings.Repeat(" ", remainingspace), topic)
	}
	insert := m.mode == Insert
	footerstyle := lipgloss.NewStyle().Foreground(Green).Reverse(insert)
	footer := footerstyle.Render(footertext)
	return fmt.Sprintf("%s\n%s", vpt, footer)
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
	part1 := "\n  %%%%%%%          "
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
	s := fmt.Sprintf("%s%s%s%s%s%s%s%s%s", style.Render(part1), text1, style.Render(part2), style.Render(part25), text2, style.Render(part3), text3, style.Render(part4), text4)

	return s
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
