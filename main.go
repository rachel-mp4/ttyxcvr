package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/charmbracelet/bubbles/list"
	tea "github.com/charmbracelet/bubbletea"
)

type txstate int

const (
	Splash txstate = iota
	Error
	GettingChannels
	ChannelList
)

type Profile struct {
	Type        string  `json:"$type"`
	Did         string  `json:"did"`
	Handle      *string `json:"handle,omitempty"`
	DisplayName *string `json:"displayName,omitempty"`
	Status      *string `json:"status,omitempty"`
	Color       *uint64 `json:"color"`
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

func (c ChannelItem) FilterValue() string {
	return c.channel.Title
}

type model struct {
	state      txstate
	width      int
	height     int
	error      *error
	command    *string
	channels   *[]Channel
	list       *list.Model
	curchannel *Channel
	color      *uint64
	nick       *string
	handle     *string
}

func initialModel() model {
	return model{
		state:  Splash,
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
	}

	return m, nil
}

func (m model) updateGettingChannels(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case channelsMsg:
		items := make([]list.Item, 0, len(msg.channels))
		for _, channel := range msg.channels {
			items = append(items, ChannelItem{channel})
		}
		list := list.New(items, list.NewDefaultDelegate(), m.width, m.height)
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
	list, cmd := m.list.Update(msg)
	m.list = &list
	return m, cmd
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
	default:
		return "under construction"
	}
}

func (m model) channelListView() string {
	return m.list.View()
}

func (m model) splashView() string {
	s := `
  %%%%%%%            t erm
 %%%%%%%%%%  %%%     x cvr  %   %           %%%%%%%%
   %%%%%%%%%%%%%%%%%        %%%%     %%%%%%%%%%%%%%%%%%
     %%%%%%%%%%%%%%%%%%%%%%% %% %%%%%%%%%%%%%%%%%%%%%%%%%%
       %%%%%%%%%%%%%%%%%%%%% %% %%%%%%%%%%%%%%%%%%%%%%%
            %%%%%%%%%%%%%%%% %% %%%%%%%%%%%%%%%%%%%%%
         %%%%%%%%%%%%%%%%%%% %% %%%%%%%%%%%%%%
       %%%%%%%%%%%%%%%%%%%%%    %%%%%%%%%%%%%%%%%%
          %%%%%%%%%%%%         %%%%%%%%%%%%%%%%
             %%%%%     made      %%%%%%%%%
                    by moth11.      %           
  					
                           press a key
                                to start!
  `

	return s
}
func main() {
	p := tea.NewProgram(initialModel())
	if _, err := p.Run(); err != nil {
		fmt.Printf("Alas, there's been an error: %v", err)
		os.Exit(1)
	}
	fmt.Println("kthxbai!")
}
