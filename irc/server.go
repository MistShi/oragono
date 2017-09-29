// Copyright (c) 2012-2014 Jeremy Latt
// Copyright (c) 2014-2015 Edmund Huber
// Copyright (c) 2016-2017 Daniel Oaks <daniel@danieloaks.net>
// released under the MIT license

package irc

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/base64"
	"fmt"
	"log"
	"math/rand"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/goshuirc/irc-go/ircfmt"
	"github.com/goshuirc/irc-go/ircmsg"
	"github.com/oragono/oragono/irc/caps"
	"github.com/oragono/oragono/irc/logger"
	"github.com/oragono/oragono/irc/sno"
	"github.com/tidwall/buntdb"
)

var (
	// common error line to sub values into
	errorMsg, _ = (&[]ircmsg.IrcMessage{ircmsg.MakeMessage(nil, "", "ERROR", "%s ")}[0]).Line()

	// common error responses
	couldNotParseIPMsg, _ = (&[]ircmsg.IrcMessage{ircmsg.MakeMessage(nil, "", "ERROR", "Unable to parse your IP address")}[0]).Line()
)

const (
	// when shutting down the REST server, wait this long
	// before killing active connections. TODO: this might not be
	// necessary at all? but it seems prudent to avoid potential resource
	// leaks
	httpShutdownTimeout = time.Second
)

// Limits holds the maximum limits for various things such as topic lengths.
type Limits struct {
	AwayLen        int
	ChannelLen     int
	KickLen        int
	MonitorEntries int
	NickLen        int
	TopicLen       int
	ChanListModes  int
	LineLen        LineLenLimits
}

// LineLenLimits holds the maximum limits for IRC lines.
type LineLenLimits struct {
	Tags int
	Rest int
}

// ListenerWrapper wraps a listener so it can be safely reconfigured or stopped
type ListenerWrapper struct {
	listener   net.Listener
	tlsConfig  *tls.Config
	shouldStop bool
	// lets the ListenerWrapper inform the server that it has stopped:
	stopEvent chan bool
	// protects atomic update of tlsConfig and shouldStop:
	configMutex sync.Mutex
}

// Server is the main Oragono server.
type Server struct {
	accountAuthenticationEnabled bool
	accountRegistration          *AccountRegistration
	accounts                     map[string]*ClientAccount
	batches                      *BatchManager
	channelRegistrationEnabled   bool
	channels                     ChannelNameMap
	channelJoinPartMutex         sync.Mutex // used when joining/parting channels to prevent stomping over each others' access and all
	checkIdent                   bool
	clients                      *ClientLookupSet
	commands                     chan Command
	configFilename               string
	configurableStateMutex       sync.RWMutex // generic protection for server state modified by rehash()
	connectionLimits             *ConnectionLimits
	connectionLimitsMutex        sync.Mutex // used when affecting the connection limiter, to make sure rehashing doesn't make things go out-of-whack
	connectionThrottle           *ConnectionThrottle
	connectionThrottleMutex      sync.Mutex // used when affecting the connection limiter, to make sure rehashing doesn't make things go out-of-whack
	ctime                        time.Time
	currentOpers                 map[*Client]bool
	defaultChannelModes          Modes
	dlines                       *DLineManager
	isupport                     *ISupportList
	klines                       *KLineManager
	limits                       Limits
	listeners                    map[string]*ListenerWrapper
	logger                       *logger.Manager
	MaxSendQBytes                uint64
	monitoring                   map[string][]*Client
	monitoringMutex              sync.RWMutex
	motdLines                    []string
	name                         string
	nameCasefolded               string
	networkName                  string
	newConns                     chan clientConn
	operators                    map[string]Oper
	operclasses                  map[string]OperClass
	password                     []byte
	passwords                    *PasswordManager
	registeredChannels           map[string]*RegisteredChannel
	registeredChannelsMutex      sync.RWMutex
	rehashMutex                  sync.Mutex
	rehashSignal                 chan os.Signal
	restAPI                      RestAPIConfig
	restAPIServer                *http.Server
	proxyAllowedFrom             []string
	signals                      chan os.Signal
	snomasks                     *SnoManager
	store                        *buntdb.DB
	stsEnabled                   bool
	whoWas                       *WhoWasList
}

var (
	// ServerExitSignals are the signals the server will exit on.
	ServerExitSignals = []os.Signal{
		syscall.SIGINT,
		syscall.SIGTERM,
		syscall.SIGQUIT,
	}
)

type clientConn struct {
	Conn  net.Conn
	IsTLS bool
}

// NewServer returns a new Oragono server.
func NewServer(config *Config, logger *logger.Manager) (*Server, error) {
	// TODO move this to main?
	if err := GenerateHelpIndices(); err != nil {
		return nil, err
	}

	// initialize data structures
	server := &Server{
		accounts:           make(map[string]*ClientAccount),
		channels:           *NewChannelNameMap(),
		clients:            NewClientLookupSet(),
		commands:           make(chan Command),
		currentOpers:       make(map[*Client]bool),
		listeners:          make(map[string]*ListenerWrapper),
		logger:             logger,
		monitoring:         make(map[string][]*Client),
		newConns:           make(chan clientConn),
		registeredChannels: make(map[string]*RegisteredChannel),
		rehashSignal:       make(chan os.Signal, 1),
		signals:            make(chan os.Signal, len(ServerExitSignals)),
		snomasks:           NewSnoManager(),
		whoWas:             NewWhoWasList(config.Limits.WhowasEntries),
	}

	if err := server.applyConfig(config, true); err != nil {
		return nil, err
	}

	// Attempt to clean up when receiving these signals.
	signal.Notify(server.signals, ServerExitSignals...)
	signal.Notify(server.rehashSignal, syscall.SIGHUP)

	return server, nil
}

// setISupport sets up our RPL_ISUPPORT reply.
func (server *Server) setISupport() {
	maxTargetsString := strconv.Itoa(maxTargets)

	// add RPL_ISUPPORT tokens
	server.isupport = NewISupportList()
	server.isupport.Add("AWAYLEN", strconv.Itoa(server.limits.AwayLen))
	server.isupport.Add("CASEMAPPING", casemappingName)
	server.isupport.Add("CHANMODES", strings.Join([]string{Modes{BanMask, ExceptMask, InviteMask}.String(), "", Modes{UserLimit, Key}.String(), Modes{InviteOnly, Moderated, NoOutside, OpOnlyTopic, ChanRoleplaying, Secret}.String()}, ","))
	server.isupport.Add("CHANNELLEN", strconv.Itoa(server.limits.ChannelLen))
	server.isupport.Add("CHANTYPES", "#")
	server.isupport.Add("ELIST", "U")
	server.isupport.Add("EXCEPTS", "")
	server.isupport.Add("INVEX", "")
	server.isupport.Add("KICKLEN", strconv.Itoa(server.limits.KickLen))
	server.isupport.Add("MAXLIST", fmt.Sprintf("beI:%s", strconv.Itoa(server.limits.ChanListModes)))
	server.isupport.Add("MAXTARGETS", maxTargetsString)
	server.isupport.Add("MODES", "")
	server.isupport.Add("MONITOR", strconv.Itoa(server.limits.MonitorEntries))
	server.isupport.Add("NETWORK", server.networkName)
	server.isupport.Add("NICKLEN", strconv.Itoa(server.limits.NickLen))
	server.isupport.Add("PREFIX", "(qaohv)~&@%+")
	server.isupport.Add("RPCHAN", "E")
	server.isupport.Add("RPUSER", "E")
	server.isupport.Add("STATUSMSG", "~&@%+")
	server.isupport.Add("TARGMAX", fmt.Sprintf("NAMES:1,LIST:1,KICK:1,WHOIS:1,USERHOST:10,PRIVMSG:%s,TAGMSG:%s,NOTICE:%s,MONITOR:", maxTargetsString, maxTargetsString, maxTargetsString))
	server.isupport.Add("TOPICLEN", strconv.Itoa(server.limits.TopicLen))

	// account registration
	if server.accountRegistration.Enabled {
		// 'none' isn't shown in the REGCALLBACKS vars
		var enabledCallbacks []string
		for _, name := range server.accountRegistration.EnabledCallbacks {
			if name != "*" {
				enabledCallbacks = append(enabledCallbacks, name)
			}
		}

		server.isupport.Add("REGCOMMANDS", "CREATE,VERIFY")
		server.isupport.Add("REGCALLBACKS", strings.Join(enabledCallbacks, ","))
		server.isupport.Add("REGCREDTYPES", "passphrase,certfp")
	}

	server.isupport.RegenerateCachedReply()
}

func loadChannelList(channel *Channel, list string, maskMode Mode) {
	if list == "" {
		return
	}
	channel.lists[maskMode].AddAll(strings.Split(list, " "))
}

// Shutdown shuts down the server.
func (server *Server) Shutdown() {
	//TODO(dan): Make sure we disallow new nicks
	server.clients.ByNickMutex.RLock()
	for _, client := range server.clients.ByNick {
		client.Notice("Server is shutting down")
	}
	server.clients.ByNickMutex.RUnlock()

	if err := server.store.Close(); err != nil {
		server.logger.Error("shutdown", fmt.Sprintln("Could not close datastore:", err))
	}
}

// Run starts the server.
func (server *Server) Run() {
	// defer closing db/store
	defer server.store.Close()

	done := false
	for !done {
		select {
		case <-server.signals:
			server.Shutdown()
			done = true

		case <-server.rehashSignal:
			server.logger.Info("rehash", "Rehashing due to SIGHUP")
			go func() {
				err := server.rehash()
				if err != nil {
					server.logger.Error("rehash", fmt.Sprintln("Failed to rehash:", err.Error()))
				}
			}()

		case conn := <-server.newConns:
			// check IP address
			ipaddr := net.ParseIP(IPString(conn.Conn.RemoteAddr()))
			if ipaddr == nil {
				conn.Conn.Write([]byte(couldNotParseIPMsg))
				conn.Conn.Close()
				continue
			}

			isBanned, banMsg := server.checkBans(ipaddr)
			if isBanned {
				// this might not show up properly on some clients, but our objective here is just to close the connection out before it has a load impact on us
				conn.Conn.Write([]byte(fmt.Sprintf(errorMsg, banMsg)))
				conn.Conn.Close()
				continue
			}

			server.logger.Debug("localconnect-ip", fmt.Sprintf("Client connecting from %v", ipaddr))
			// prolly don't need to alert snomasks on this, only on connection reg

			go NewClient(server, conn.Conn, conn.IsTLS)
			continue
		}
	}
}

func (server *Server) checkBans(ipaddr net.IP) (banned bool, message string) {
	// check DLINEs
	isBanned, info := server.dlines.CheckIP(ipaddr)
	if isBanned {
		return true, info.BanMessage("You are banned from this server (%s)")
	}

	// check connection limits
	server.connectionLimitsMutex.Lock()
	err := server.connectionLimits.AddClient(ipaddr, false)
	server.connectionLimitsMutex.Unlock()
	if err != nil {
		// too many connections from one client, tell the client and close the connection
		return true, "Too many clients from your network"
	}

	// check connection throttle
	server.connectionThrottleMutex.Lock()
	err = server.connectionThrottle.AddClient(ipaddr)
	server.connectionThrottleMutex.Unlock()
	if err != nil {
		// too many connections too quickly from client, tell them and close the connection
		length := &IPRestrictTime{
			Duration: server.connectionThrottle.BanDuration,
			Expires:  time.Now().Add(server.connectionThrottle.BanDuration),
		}
		server.dlines.AddIP(ipaddr, length, server.connectionThrottle.BanMessage, "Exceeded automated connection throttle")

		// they're DLINE'd for 15 minutes or whatever, so we can reset the connection throttle now,
		// and once their temporary DLINE is finished they can fill up the throttler again
		server.connectionThrottle.ResetFor(ipaddr)

		// this might not show up properly on some clients, but our objective here is just to close it out before it has a load impact on us
		return true, server.connectionThrottle.BanMessage
	}

	return false, ""
}

//
// IRC protocol listeners
//

// createListener starts the given listeners.
func (server *Server) createListener(addr string, tlsConfig *tls.Config) *ListenerWrapper {
	// make listener
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		log.Fatal(server, "listen error: ", err)
	}

	// throw our details to the server so we can be modified/killed later
	wrapper := ListenerWrapper{
		listener:   listener,
		tlsConfig:  tlsConfig,
		shouldStop: false,
		stopEvent:  make(chan bool, 1),
	}

	var shouldStop bool

	// setup accept goroutine
	go func() {
		for {
			conn, err := listener.Accept()

			// synchronously access config data:
			// whether TLS is enabled and whether we should stop listening
			wrapper.configMutex.Lock()
			shouldStop = wrapper.shouldStop
			tlsConfig = wrapper.tlsConfig
			wrapper.configMutex.Unlock()

			if err == nil {
				if tlsConfig != nil {
					conn = tls.Server(conn, tlsConfig)
				}
				newConn := clientConn{
					Conn:  conn,
					IsTLS: tlsConfig != nil,
				}
				// hand off the connection
				server.newConns <- newConn
			}

			if shouldStop {
				listener.Close()
				wrapper.stopEvent <- true
				return
			}
		}
	}()

	return &wrapper
}

// generateMessageID returns a network-unique message ID.
func (server *Server) generateMessageID() string {
	return fmt.Sprintf("%s-%s", strconv.FormatInt(time.Now().UTC().UnixNano(), 10), strconv.FormatInt(rand.Int63(), 10))
}

//
// server functionality
//

func (server *Server) tryRegister(c *Client) {
	if c.registered || !c.HasNick() || !c.HasUsername() ||
		(c.capState == CapNegotiating) {
		return
	}

	// check KLINEs
	isBanned, info := server.klines.CheckMasks(c.AllNickmasks()...)
	if isBanned {
		reason := info.Reason
		if info.Time != nil {
			reason += fmt.Sprintf(" [%s]", info.Time.Duration.String())
		}
		c.Send(nil, "", "ERROR", fmt.Sprintf("You are banned from this server (%s)", reason))
		c.quitMessageSent = true
		c.destroy()
		return
	}

	// continue registration
	server.logger.Debug("localconnect", fmt.Sprintf("Client registered [%s] [u:%s] [r:%s]", c.nick, c.username, c.realname))
	server.snomasks.Send(sno.LocalConnects, fmt.Sprintf(ircfmt.Unescape("Client registered $c[grey][$r%s$c[grey]] [u:$r%s$c[grey]] [h:$r%s$c[grey]] [r:$r%s$c[grey]]"), c.nick, c.username, c.rawHostname, c.realname))
	c.Register()

	// send welcome text
	//NOTE(dan): we specifically use the NICK here instead of the nickmask
	// see http://modern.ircdocs.horse/#rplwelcome-001 for details on why we avoid using the nickmask
	c.Send(nil, server.name, RPL_WELCOME, c.nick, fmt.Sprintf("Welcome to the Internet Relay Network %s", c.nick))
	c.Send(nil, server.name, RPL_YOURHOST, c.nick, fmt.Sprintf("Your host is %s, running version %s", server.name, Ver))
	c.Send(nil, server.name, RPL_CREATED, c.nick, fmt.Sprintf("This server was created %s", server.ctime.Format(time.RFC1123)))
	//TODO(dan): Look at adding last optional [<channel modes with a parameter>] parameter
	c.Send(nil, server.name, RPL_MYINFO, c.nick, server.name, Ver, supportedUserModesString, supportedChannelModesString)
	c.RplISupport()
	server.MOTD(c)
	c.Send(nil, c.nickMaskString, RPL_UMODEIS, c.nick, c.ModeString())
	if server.logger.DumpingRawInOut {
		c.Notice("This server is in debug mode and is logging all user I/O. If you do not wish for everything you send to be readable by the server owner(s), please disconnect.")
	}
}

// MOTD serves the Message of the Day.
func (server *Server) MOTD(client *Client) {
	server.configurableStateMutex.RLock()
	defer server.configurableStateMutex.RUnlock()

	if len(server.motdLines) < 1 {
		client.Send(nil, server.name, ERR_NOMOTD, client.nick, "MOTD File is missing")
		return
	}

	client.Send(nil, server.name, RPL_MOTDSTART, client.nick, fmt.Sprintf("- %s Message of the day - ", server.name))
	for _, line := range server.motdLines {
		client.Send(nil, server.name, RPL_MOTD, client.nick, line)
	}
	client.Send(nil, server.name, RPL_ENDOFMOTD, client.nick, "End of MOTD command")
}

//
// registration commands
//

// PASS <password>
func passHandler(server *Server, client *Client, msg ircmsg.IrcMessage) bool {
	if client.registered {
		client.Send(nil, server.name, ERR_ALREADYREGISTRED, client.nick, "You may not reregister")
		return false
	}

	// if no password exists, skip checking
	if len(server.password) == 0 {
		client.authorized = true
		return false
	}

	// check the provided password
	password := []byte(msg.Params[0])
	if ComparePassword(server.password, password) != nil {
		client.Send(nil, server.name, ERR_PASSWDMISMATCH, client.nick, "Password incorrect")
		client.Send(nil, server.name, "ERROR", "Password incorrect")
		return true
	}

	client.authorized = true
	return false
}

// USER <username> * 0 <realname>
func userHandler(server *Server, client *Client, msg ircmsg.IrcMessage) bool {
	if client.registered {
		client.Send(nil, server.name, ERR_ALREADYREGISTRED, client.nick, "You may not reregister")
		return false
	}

	if !client.authorized {
		client.Quit("Bad password")
		return true
	}

	if client.username != "" && client.realname != "" {
		return false
	}

	// confirm that username is valid
	//
	_, err := CasefoldName(msg.Params[0])
	if err != nil {
		client.Send(nil, "", "ERROR", "Malformed username")
		return true
	}

	if !client.HasUsername() {
		client.username = "~" + msg.Params[0]
		// don't bother updating nickmask here, it's not valid anyway
	}
	if client.realname == "" {
		client.realname = msg.Params[3]
	}

	server.tryRegister(client)

	return false
}

// QUIT [<reason>]
func quitHandler(server *Server, client *Client, msg ircmsg.IrcMessage) bool {
	reason := "Quit"
	if len(msg.Params) > 0 {
		reason += ": " + msg.Params[0]
	}
	client.Quit(reason)
	return true
}

//
// normal commands
//

// PING <server1> [<server2>]
func pingHandler(server *Server, client *Client, msg ircmsg.IrcMessage) bool {
	client.Send(nil, server.name, "PONG", msg.Params...)
	return false
}

// PONG <server> [ <server2> ]
func pongHandler(server *Server, client *Client, msg ircmsg.IrcMessage) bool {
	// client gets touched when they send this command, so we don't need to do anything
	return false
}

// RENAME <oldchan> <newchan> [<reason>]
//TODO(dan): Clean up this function so it doesn't look like an eldrich horror... prolly by putting it into a server.renameChannel function.
func renameHandler(server *Server, client *Client, msg ircmsg.IrcMessage) bool {
	// get lots of locks... make sure nobody touches anything while we're doing this
	server.registeredChannelsMutex.Lock()
	defer server.registeredChannelsMutex.Unlock()
	server.channels.ChansLock.Lock()
	defer server.channels.ChansLock.Unlock()

	oldName := strings.TrimSpace(msg.Params[0])
	newName := strings.TrimSpace(msg.Params[1])
	reason := "No reason"
	if 2 < len(msg.Params) {
		reason = msg.Params[2]
	}

	// check for all the reasons why the rename couldn't happen
	casefoldedOldName, err := CasefoldChannel(oldName)
	if err != nil {
		//TODO(dan): Change this to ERR_CANNOTRENAME
		client.Send(nil, server.name, ERR_UNKNOWNERROR, client.nick, "RENAME", oldName, "Old channel name is invalid")
		return false
	}

	channel := server.channels.Chans[casefoldedOldName]
	if channel == nil {
		client.Send(nil, server.name, ERR_NOSUCHCHANNEL, client.nick, oldName, "No such channel")
		return false
	}

	//TODO(dan): allow IRCops to do this?
	if !channel.ClientIsAtLeast(client, Operator) {
		client.Send(nil, server.name, ERR_CHANOPRIVSNEEDED, client.nick, oldName, "Only chanops can rename channels")
		return false
	}

	channel.membersMutex.Lock()
	defer channel.membersMutex.Unlock()

	casefoldedNewName, err := CasefoldChannel(newName)
	if err != nil {
		//TODO(dan): Change this to ERR_CANNOTRENAME
		client.Send(nil, server.name, ERR_UNKNOWNERROR, client.nick, "RENAME", newName, "New channel name is invalid")
		return false
	}

	newChannel := server.channels.Chans[casefoldedNewName]
	if newChannel != nil {
		//TODO(dan): Change this to ERR_CHANNAMEINUSE
		client.Send(nil, server.name, ERR_UNKNOWNERROR, client.nick, "RENAME", newName, "New channel name is in use")
		return false
	}

	var canEdit bool
	server.store.Update(func(tx *buntdb.Tx) error {
		chanReg := server.loadChannelNoMutex(tx, casefoldedOldName)
		if chanReg == nil || !client.LoggedIntoAccount() || client.account.Name == chanReg.Founder {
			canEdit = true
		}

		chanReg = server.loadChannelNoMutex(tx, casefoldedNewName)
		if chanReg != nil {
			canEdit = false
		}
		return nil
	})
	if !canEdit {
		//TODO(dan): Change this to ERR_CANNOTRENAME
		client.Send(nil, server.name, ERR_UNKNOWNERROR, client.nick, "RENAME", oldName, "Only channel founders can change registered channels")
		return false
	}

	// perform the channel rename
	server.channels.Chans[casefoldedOldName] = nil
	server.channels.Chans[casefoldedNewName] = channel

	channel.name = strings.TrimSpace(msg.Params[1])
	channel.nameCasefolded = casefoldedNewName

	// rename stored channel info if any exists
	server.store.Update(func(tx *buntdb.Tx) error {
		chanReg := server.loadChannelNoMutex(tx, casefoldedOldName)
		if chanReg == nil {
			return nil
		}

		server.deleteChannelNoMutex(tx, casefoldedOldName)

		chanReg.Name = newName

		server.saveChannelNoMutex(tx, casefoldedNewName, *chanReg)
		return nil
	})

	// send RENAME messages
	for mcl := range channel.members {
		if mcl.capabilities[caps.Rename] {
			mcl.Send(nil, client.nickMaskString, "RENAME", oldName, newName, reason)
		} else {
			mcl.Send(nil, mcl.nickMaskString, "PART", oldName, fmt.Sprintf("Channel renamed: %s", reason))
			if mcl.capabilities[caps.ExtendedJoin] {
				accountName := "*"
				if mcl.account != nil {
					accountName = mcl.account.Name
				}
				mcl.Send(nil, mcl.nickMaskString, "JOIN", newName, accountName, mcl.realname)
			} else {
				mcl.Send(nil, mcl.nickMaskString, "JOIN", newName)
			}
		}
	}

	return false
}

// JOIN <channel>{,<channel>} [<key>{,<key>}]
func joinHandler(server *Server, client *Client, msg ircmsg.IrcMessage) bool {
	// kill JOIN 0 requests
	if msg.Params[0] == "0" {
		client.Notice("JOIN 0 is not allowed")
		return false
	}

	// handle regular JOINs
	channels := strings.Split(msg.Params[0], ",")
	var keys []string
	if len(msg.Params) > 1 {
		keys = strings.Split(msg.Params[1], ",")
	}

	// get lock
	server.channelJoinPartMutex.Lock()
	defer server.channelJoinPartMutex.Unlock()

	for i, name := range channels {
		casefoldedName, err := CasefoldChannel(name)
		if err != nil {
			if len(name) > 0 {
				client.Send(nil, server.name, ERR_NOSUCHCHANNEL, client.nick, name, "No such channel")
			}
			continue
		}

		channel := server.channels.Get(casefoldedName)
		if channel == nil {
			if len(casefoldedName) > server.limits.ChannelLen {
				client.Send(nil, server.name, ERR_NOSUCHCHANNEL, client.nick, name, "No such channel")
				continue
			}
			channel = NewChannel(server, name, true)
		}

		var key string
		if len(keys) > i {
			key = keys[i]
		}

		channel.Join(client, key)
	}
	return false
}

// PART <channel>{,<channel>} [<reason>]
func partHandler(server *Server, client *Client, msg ircmsg.IrcMessage) bool {
	channels := strings.Split(msg.Params[0], ",")
	var reason string //TODO(dan): if this isn't supplied here, make sure the param doesn't exist in the PART message sent to other users
	if len(msg.Params) > 1 {
		reason = msg.Params[1]
	}

	// get lock
	server.channelJoinPartMutex.Lock()
	defer server.channelJoinPartMutex.Unlock()

	for _, chname := range channels {
		casefoldedChannelName, err := CasefoldChannel(chname)
		channel := server.channels.Get(casefoldedChannelName)

		if err != nil || channel == nil {
			if len(chname) > 0 {
				client.Send(nil, server.name, ERR_NOSUCHCHANNEL, client.nick, chname, "No such channel")
			}
			continue
		}

		channel.Part(client, reason)
	}
	return false
}

// TOPIC <channel> [<topic>]
func topicHandler(server *Server, client *Client, msg ircmsg.IrcMessage) bool {
	name, err := CasefoldChannel(msg.Params[0])
	channel := server.channels.Get(name)
	if err != nil || channel == nil {
		if len(msg.Params[0]) > 0 {
			client.Send(nil, server.name, ERR_NOSUCHCHANNEL, client.nick, msg.Params[0], "No such channel")
		}
		return false
	}

	if len(msg.Params) > 1 {
		channel.SetTopic(client, msg.Params[1])
	} else {
		channel.GetTopic(client)
	}
	return false
}

// wordWrap wraps the given text into a series of lines that don't exceed lineWidth characters.
func wordWrap(text string, lineWidth int) []string {
	var lines []string
	var cacheLine, cacheWord string

	for _, char := range text {
		if char == '\r' {
			continue
		} else if char == '\n' {
			cacheLine += cacheWord
			lines = append(lines, cacheLine)
			cacheWord = ""
			cacheLine = ""
		} else if (char == ' ' || char == '-') && len(cacheLine)+len(cacheWord)+1 < lineWidth {
			// natural word boundary
			cacheLine += cacheWord + string(char)
			cacheWord = ""
		} else if lineWidth <= len(cacheLine)+len(cacheWord)+1 {
			// time to wrap to next line
			if len(cacheLine) < (lineWidth / 2) {
				// this word takes up more than half a line... just split in the middle of the word
				cacheLine += cacheWord + string(char)
				cacheWord = ""
			} else {
				cacheWord += string(char)
			}
			lines = append(lines, cacheLine)
			cacheLine = ""
		} else {
			// normal character
			cacheWord += string(char)
		}
	}
	if 0 < len(cacheWord) {
		cacheLine += cacheWord
	}
	if 0 < len(cacheLine) {
		lines = append(lines, cacheLine)
	}

	return lines
}

// SplitMessage represents a message that's been split for sending.
type SplitMessage struct {
	For512     []string
	ForMaxLine string
}

func (server *Server) splitMessage(original string, origIs512 bool) SplitMessage {
	var newSplit SplitMessage

	newSplit.ForMaxLine = original

	if !origIs512 {
		newSplit.For512 = wordWrap(original, 400)
	} else {
		newSplit.For512 = []string{original}
	}

	return newSplit
}

// PRIVMSG <target>{,<target>} <message>
func privmsgHandler(server *Server, client *Client, msg ircmsg.IrcMessage) bool {
	clientOnlyTags := GetClientOnlyTags(msg.Tags)
	targets := strings.Split(msg.Params[0], ",")
	message := msg.Params[1]

	// split privmsg
	splitMsg := server.splitMessage(message, !client.capabilities[caps.MaxLine])

	for i, targetString := range targets {
		// max of four targets per privmsg
		if i > maxTargets-1 {
			break
		}
		prefixes, targetString := SplitChannelMembershipPrefixes(targetString)
		lowestPrefix := GetLowestChannelModePrefix(prefixes)

		// eh, no need to notify them
		if len(targetString) < 1 {
			continue
		}

		// get label and apply to response
		var label string
		labelValue, exists := msg.Tags["draft/label"]
		if exists && labelValue.Value != "" {
			label = labelValue.Value
		}

		target, err := CasefoldChannel(targetString)
		if err == nil {
			channel := server.channels.Get(target)
			if channel == nil {
				client.Send(nil, server.name, ERR_NOSUCHCHANNEL, client.nick, targetString, "No such channel")
				continue
			}
			if !channel.CanSpeak(client) {
				client.Send(nil, client.server.name, ERR_CANNOTSENDTOCHAN, channel.name, "Cannot send to channel")
				continue
			}
			msgid := server.generateMessageID()
			channel.SplitPrivMsg(msgid, label, lowestPrefix, clientOnlyTags, client, splitMsg)
		} else {
			target, err = CasefoldName(targetString)
			if target == "chanserv" {
				server.chanservReceivePrivmsg(client, message)
				continue
			} else if target == "nickserv" {
				server.nickservReceivePrivmsg(client, message)
				continue
			}
			user := server.clients.Get(target)
			if err != nil || user == nil {
				if len(target) > 0 {
					client.Send(nil, server.name, ERR_NOSUCHNICK, client.nick, target, "No such nick")
				}
				continue
			}
			if !user.capabilities[caps.MessageTags] {
				clientOnlyTags = nil
			}
			msgid := server.generateMessageID()
			// restrict messages appropriately when +R is set
			// intentionally make the sending user think the message went through fine
			if !user.flags[RegisteredOnly] || client.registered {
				user.SendSplitMsgFromClient(msgid, "", client, clientOnlyTags, "PRIVMSG", user.nick, splitMsg)
			}

			if client.capabilities[caps.EchoMessage] {
				client.SendSplitMsgFromClient(msgid, label, client, clientOnlyTags, "PRIVMSG", user.nick, splitMsg)
			}

			if user.flags[Away] {
				//TODO(dan): implement cooldown of away notifications to users
				client.Send(nil, server.name, RPL_AWAY, user.nick, user.awayMessage)
			}
		}
	}
	return false
}

// TAGMSG <target>{,<target>}
func tagmsgHandler(server *Server, client *Client, msg ircmsg.IrcMessage) bool {
	clientOnlyTags := GetClientOnlyTags(msg.Tags)
	// no client-only tags, so we can drop it
	if clientOnlyTags == nil {
		return false
	}

	targets := strings.Split(msg.Params[0], ",")

	for i, targetString := range targets {
		// max of four targets per privmsg
		if i > maxTargets-1 {
			break
		}
		prefixes, targetString := SplitChannelMembershipPrefixes(targetString)
		lowestPrefix := GetLowestChannelModePrefix(prefixes)

		// eh, no need to notify them
		if len(targetString) < 1 {
			continue
		}

		// get label and apply to response
		var label string
		labelValue, exists := msg.Tags["draft/label"]
		if exists && labelValue.Value != "" {
			label = labelValue.Value
		}

		target, err := CasefoldChannel(targetString)
		if err == nil {
			channel := server.channels.Get(target)
			if channel == nil {
				client.Send(nil, server.name, ERR_NOSUCHCHANNEL, client.nick, targetString, "No such channel")
				continue
			}
			if !channel.CanSpeak(client) {
				client.Send(nil, client.server.name, ERR_CANNOTSENDTOCHAN, channel.name, "Cannot send to channel")
				continue
			}
			msgid := server.generateMessageID()

			channel.TagMsg(msgid, label, lowestPrefix, clientOnlyTags, client)
		} else {
			target, err = CasefoldName(targetString)
			user := server.clients.Get(target)
			if err != nil || user == nil {
				if len(target) > 0 {
					client.Send(nil, server.name, ERR_NOSUCHNICK, client.nick, target, "No such nick")
				}
				continue
			}
			msgid := server.generateMessageID()

			// end user can't receive tagmsgs
			if !user.capabilities[caps.MessageTags] {
				continue
			}
			user.SendFromClient("", msgid, "", client, clientOnlyTags, "TAGMSG", user.nick)

			if client.capabilities[caps.EchoMessage] {
				client.SendFromClient("", msgid, label, client, clientOnlyTags, "TAGMSG", user.nick)
			}
			if user.flags[Away] {
				//TODO(dan): possibly implement cooldown of away notifications to users
				client.Send(nil, server.name, RPL_AWAY, user.nick, user.awayMessage)
			}
		}
	}
	return false
}

// WhoisChannelsNames returns the common channel names between two users.
func (client *Client) WhoisChannelsNames(target *Client) []string {
	isMultiPrefix := target.capabilities[caps.MultiPrefix]
	var chstrs []string
	index := 0
	for channel := range client.channels {
		channel.membersMutex.RLock()
		defer channel.membersMutex.RUnlock()

		// channel is secret and the target can't see it
		if !target.flags[Operator] && channel.flags[Secret] && !channel.members.Has(target) {
			continue
		}
		chstrs = append(chstrs, channel.members[client].Prefixes(isMultiPrefix)+channel.name)
		index++
	}
	return chstrs
}

// WHOIS [ <target> ] <mask> *( "," <mask> )
func whoisHandler(server *Server, client *Client, msg ircmsg.IrcMessage) bool {
	var masksString string
	//var target string

	if len(msg.Params) > 1 {
		//target = msg.Params[0]
		masksString = msg.Params[1]
	} else {
		masksString = msg.Params[0]
	}

	if len(strings.TrimSpace(masksString)) < 1 {
		client.Send(nil, server.name, ERR_UNKNOWNERROR, client.nick, msg.Command, "No masks given")
		return false
	}

	if client.flags[Operator] {
		masks := strings.Split(masksString, ",")
		for _, mask := range masks {
			casefoldedMask, err := Casefold(mask)
			if err != nil {
				client.Send(nil, client.server.name, ERR_NOSUCHNICK, client.nick, mask, "No such nick")
				continue
			}
			matches := server.clients.FindAll(casefoldedMask)
			if len(matches) == 0 {
				client.Send(nil, client.server.name, ERR_NOSUCHNICK, client.nick, mask, "No such nick")
				continue
			}
			for mclient := range matches {
				client.getWhoisOf(mclient)
			}
		}
	} else {
		// only get the first request
		casefoldedMask, err := Casefold(strings.Split(masksString, ",")[0])
		mclient := server.clients.Get(casefoldedMask)
		if err != nil || mclient == nil {
			client.Send(nil, client.server.name, ERR_NOSUCHNICK, client.nick, masksString, "No such nick")
			// fall through, ENDOFWHOIS is always sent
		} else {
			client.getWhoisOf(mclient)
		}
	}
	client.Send(nil, server.name, RPL_ENDOFWHOIS, client.nick, masksString, "End of /WHOIS list")
	return false
}

func (client *Client) getWhoisOf(target *Client) {
	client.Send(nil, client.server.name, RPL_WHOISUSER, client.nick, target.nick, target.username, target.hostname, "*", target.realname)

	whoischannels := client.WhoisChannelsNames(target)
	if whoischannels != nil {
		client.Send(nil, client.server.name, RPL_WHOISCHANNELS, client.nick, target.nick, strings.Join(whoischannels, " "))
	}
	if target.class != nil {
		client.Send(nil, client.server.name, RPL_WHOISOPERATOR, client.nick, target.nick, target.whoisLine)
	}
	if client.flags[Operator] || client == target {
		client.Send(nil, client.server.name, RPL_WHOISACTUALLY, client.nick, target.nick, fmt.Sprintf("%s@%s", target.username, LookupHostname(target.IPString())), target.IPString(), "Actual user@host, Actual IP")
	}
	if target.flags[TLS] {
		client.Send(nil, client.server.name, RPL_WHOISSECURE, client.nick, target.nick, "is using a secure connection")
	}
	if target.certfp != "" && (client.flags[Operator] || client == target) {
		client.Send(nil, client.server.name, RPL_WHOISCERTFP, client.nick, target.nick, fmt.Sprintf("has client certificate fingerprint %s", target.certfp))
	}
	client.Send(nil, client.server.name, RPL_WHOISIDLE, client.nick, target.nick, strconv.FormatUint(target.IdleSeconds(), 10), strconv.FormatInt(target.SignonTime(), 10), "seconds idle, signon time")
}

// RplWhoReplyNoMutex returns the WHO reply between one user and another channel/user.
// <channel> <user> <host> <server> <nick> ( "H" / "G" ) ["*"] [ ( "@" / "+" ) ]
// :<hopcount> <real name>
func (target *Client) RplWhoReplyNoMutex(channel *Channel, client *Client) {
	channelName := "*"
	flags := ""

	if client.flags[Away] {
		flags = "G"
	} else {
		flags = "H"
	}
	if client.flags[Operator] {
		flags += "*"
	}

	if channel != nil {
		flags += channel.members[client].Prefixes(target.capabilities[caps.MultiPrefix])
		channelName = channel.name
	}
	target.Send(nil, target.server.name, RPL_WHOREPLY, target.nick, channelName, client.username, client.hostname, client.server.name, client.nick, flags, strconv.Itoa(client.hops)+" "+client.realname)
}

func whoChannel(client *Client, channel *Channel, friends ClientSet) {
	channel.membersMutex.RLock()
	defer channel.membersMutex.RUnlock()

	for member := range channel.members {
		if !client.flags[Invisible] || friends[client] {
			client.RplWhoReplyNoMutex(channel, member)
		}
	}
}

// WHO [ <mask> [ "o" ] ]
func whoHandler(server *Server, client *Client, msg ircmsg.IrcMessage) bool {
	friends := client.Friends()

	var mask string
	if len(msg.Params) > 0 {
		casefoldedMask, err := Casefold(msg.Params[0])
		if err != nil {
			client.Send(nil, server.name, ERR_UNKNOWNERROR, "WHO", "Mask isn't valid")
			return false
		}
		mask = casefoldedMask
	}

	//TODO(dan): is this used and would I put this param in the Modern doc?
	// if not, can we remove it?
	//var operatorOnly bool
	//if len(msg.Params) > 1 && msg.Params[1] == "o" {
	//	operatorOnly = true
	//}

	if mask == "" {
		server.channels.ChansLock.RLock()
		for _, channel := range server.channels.Chans {
			whoChannel(client, channel, friends)
		}
		server.channels.ChansLock.RUnlock()
	} else if mask[0] == '#' {
		// TODO implement wildcard matching
		//TODO(dan): ^ only for opers
		channel := server.channels.Get(mask)
		if channel != nil {
			whoChannel(client, channel, friends)
		}
	} else {
		for mclient := range server.clients.FindAll(mask) {
			client.RplWhoReplyNoMutex(nil, mclient)
		}
	}

	client.Send(nil, server.name, RPL_ENDOFWHO, client.nick, mask, "End of WHO list")
	return false
}

// OPER <name> <password>
func operHandler(server *Server, client *Client, msg ircmsg.IrcMessage) bool {
	name, err := CasefoldName(msg.Params[0])
	if err != nil {
		client.Send(nil, server.name, ERR_PASSWDMISMATCH, client.nick, "Password incorrect")
		return true
	}
	if client.flags[Operator] == true {
		client.Send(nil, server.name, ERR_UNKNOWNERROR, "OPER", "You're already opered-up!")
		return false
	}
	hash := server.operators[name].Pass
	password := []byte(msg.Params[1])

	err = ComparePassword(hash, password)

	if (hash == nil) || (err != nil) {
		client.Send(nil, server.name, ERR_PASSWDMISMATCH, client.nick, "Password incorrect")
		return true
	}

	client.flags[Operator] = true
	client.operName = name
	client.class = server.operators[name].Class
	server.currentOpers[client] = true
	client.whoisLine = server.operators[name].WhoisLine

	// push new vhost if one is set
	if len(server.operators[name].Vhost) > 0 {
		for fClient := range client.Friends(caps.ChgHost) {
			fClient.SendFromClient("", "", "", client, nil, "CHGHOST", client.username, server.operators[name].Vhost)
		}
		// CHGHOST requires prefix nickmask to have original hostname, so do that before updating nickmask
		client.vhost = server.operators[name].Vhost
		client.updateNickMask()
	}

	// set new modes
	var applied ModeChanges
	if 0 < len(server.operators[name].Modes) {
		modeChanges, unknownChanges := ParseUserModeChanges(strings.Split(server.operators[name].Modes, " ")...)
		applied = client.applyUserModeChanges(true, modeChanges)
		if 0 < len(unknownChanges) {
			var runes string
			for r := range unknownChanges {
				runes += string(r)
			}
			client.Notice(fmt.Sprintf("Could not apply mode changes: +%s", runes))
		}
	}

	client.Send(nil, server.name, RPL_YOUREOPER, client.nick, "You are now an IRC operator")

	applied = append(applied, ModeChange{
		mode: Operator,
		op:   Add,
	})
	client.Send(nil, server.name, "MODE", client.nick, applied.String())

	server.snomasks.Send(sno.LocalOpers, fmt.Sprintf(ircfmt.Unescape("Client opered up $c[grey][$r%s$c[grey], $r%s$c[grey]]"), client.nickMaskString, client.operName))
	return false
}

// rehash reloads the config and applies the changes from the config file.
func (server *Server) rehash() error {
	server.logger.Debug("rehash", "Starting rehash")

	// only let one REHASH go on at a time
	server.rehashMutex.Lock()
	defer server.rehashMutex.Unlock()

	server.logger.Debug("rehash", "Got rehash lock")

	config, err := LoadConfig(server.configFilename)
	if err != nil {
		return fmt.Errorf("Error loading config file config: %s", err.Error())
	}

	err = server.applyConfig(config, false)
	if err != nil {
		return fmt.Errorf("Error applying config changes: %s", err.Error())
	}

	return nil
}

func (server *Server) applyConfig(config *Config, initial bool) error {
	if initial {
		server.ctime = time.Now()
		server.configFilename = config.Filename
	} else {
		// enforce configs that can't be changed after launch:
		if server.limits.LineLen.Tags != config.Limits.LineLen.Tags || server.limits.LineLen.Rest != config.Limits.LineLen.Rest {
			return fmt.Errorf("Maximum line length (linelen) cannot be changed after launching the server, rehash aborted")
		} else if server.name != config.Server.Name {
			return fmt.Errorf("Server name cannot be changed after launching the server, rehash aborted")
		}
	}

	casefoldedName, err := Casefold(config.Server.Name)
	if err != nil {
		return fmt.Errorf("Server name isn't valid [%s]: %s", config.Server.Name, err.Error())
	}

	// confirm connectionLimits are fine
	connectionLimits, err := NewConnectionLimits(config.Server.ConnectionLimits)
	if err != nil {
		return fmt.Errorf("Error rehashing config file connection-limits: %s", err.Error())
	}

	// confirm connectionThrottler is fine
	connectionThrottle, err := NewConnectionThrottle(config.Server.ConnectionThrottle)
	if err != nil {
		return fmt.Errorf("Error rehashing config file connection-throttle: %s", err.Error())
	}

	// confirm operator stuff all exists and is fine
	operclasses, err := config.OperatorClasses()
	if err != nil {
		return fmt.Errorf("Error rehashing config file operclasses: %s", err.Error())
	}
	opers, err := config.Operators(operclasses)
	if err != nil {
		return fmt.Errorf("Error rehashing config file opers: %s", err.Error())
	}
	for client := range server.currentOpers {
		_, exists := opers[client.operName]
		if !exists {
			return fmt.Errorf("Oper [%s] no longer exists (used by client [%s])", client.operName, client.nickMaskString)
		}
	}

	// sanity checks complete, start modifying server state

	server.name = config.Server.Name
	server.nameCasefolded = casefoldedName
	server.networkName = config.Network.Name

	if config.Server.Password != "" {
		server.password = config.Server.PasswordBytes()
	} else {
		server.password = nil
	}

	// apply new PROXY command restrictions
	server.proxyAllowedFrom = config.Server.ProxyAllowedFrom

	// apply new connectionlimits
	server.connectionLimitsMutex.Lock()
	server.connectionLimits = connectionLimits
	server.connectionThrottleMutex.Lock()
	server.connectionThrottle = connectionThrottle

	server.clients.ByNickMutex.RLock()
	for _, client := range server.clients.ByNick {
		ipaddr := client.IP()
		if ipaddr != nil {
			server.connectionLimits.AddClient(ipaddr, true)
		}
	}
	server.clients.ByNickMutex.RUnlock()
	server.connectionThrottleMutex.Unlock()
	server.connectionLimitsMutex.Unlock()

	// setup new and removed caps
	addedCaps := make(CapabilitySet)
	removedCaps := make(CapabilitySet)
	updatedCaps := make(CapabilitySet)

	// SASL
	if config.Accounts.AuthenticationEnabled && !server.accountAuthenticationEnabled {
		// enabling SASL
		SupportedCapabilities[caps.SASL] = true
		addedCaps[caps.SASL] = true
	}
	if !config.Accounts.AuthenticationEnabled && server.accountAuthenticationEnabled {
		// disabling SASL
		SupportedCapabilities[caps.SASL] = false
		removedCaps[caps.SASL] = true
	}
	server.accountAuthenticationEnabled = config.Accounts.AuthenticationEnabled

	// STS
	stsValue := config.Server.STS.Value()
	var stsDisabled bool
	server.logger.Debug("rehash", "STS Vals", CapValues[caps.STS], stsValue, fmt.Sprintf("server[%v] config[%v]", server.stsEnabled, config.Server.STS.Enabled))
	if config.Server.STS.Enabled && !server.stsEnabled {
		// enabling STS
		SupportedCapabilities[caps.STS] = true
		addedCaps[caps.STS] = true
		CapValues[caps.STS] = stsValue
	} else if !config.Server.STS.Enabled && server.stsEnabled {
		// disabling STS
		SupportedCapabilities[caps.STS] = false
		removedCaps[caps.STS] = true
		stsDisabled = true
	} else if config.Server.STS.Enabled && server.stsEnabled && stsValue != CapValues[caps.STS] {
		// STS policy updated
		CapValues[caps.STS] = stsValue
		updatedCaps[caps.STS] = true
	}
	server.stsEnabled = config.Server.STS.Enabled

	// burst new and removed caps
	var capBurstClients ClientSet
	added := make(map[CapVersion]string)
	var removed string

	// updated caps get DEL'd and then NEW'd
	// so, we can just add updated ones to both removed and added lists here and they'll be correctly handled
	server.logger.Debug("rehash", "Updated Caps", updatedCaps.String(Cap301), strconv.Itoa(len(updatedCaps)))
	if len(updatedCaps) > 0 {
		for capab := range updatedCaps {
			addedCaps[capab] = true
			removedCaps[capab] = true
		}
	}

	if len(addedCaps) > 0 || len(removedCaps) > 0 {
		capBurstClients = server.clients.AllWithCaps(caps.CapNotify)

		added[Cap301] = addedCaps.String(Cap301)
		added[Cap302] = addedCaps.String(Cap302)
		// removed never has values
		removed = removedCaps.String(Cap301)
	}

	for sClient := range capBurstClients {
		if stsDisabled {
			// remove STS policy
			//TODO(dan): this is an ugly hack. we can write this better.
			stsPolicy := "sts=duration=0"
			if len(addedCaps) > 0 {
				added[Cap302] = added[Cap302] + " " + stsPolicy
			} else {
				addedCaps[caps.STS] = true
				added[Cap302] = stsPolicy
			}
		}
		// DEL caps and then send NEW ones so that updated caps get removed/added correctly
		if len(removedCaps) > 0 {
			sClient.Send(nil, server.name, "CAP", sClient.nick, "DEL", removed)
		}
		if len(addedCaps) > 0 {
			sClient.Send(nil, server.name, "CAP", sClient.nick, "NEW", added[sClient.capVersion])
		}
	}

	// set server options
	lineLenConfig := LineLenLimits{
		Tags: config.Limits.LineLen.Tags,
		Rest: config.Limits.LineLen.Rest,
	}
	server.limits = Limits{
		AwayLen:        int(config.Limits.AwayLen),
		ChannelLen:     int(config.Limits.ChannelLen),
		KickLen:        int(config.Limits.KickLen),
		MonitorEntries: int(config.Limits.MonitorEntries),
		NickLen:        int(config.Limits.NickLen),
		TopicLen:       int(config.Limits.TopicLen),
		ChanListModes:  int(config.Limits.ChanListModes),
		LineLen:        lineLenConfig,
	}
	server.operclasses = *operclasses
	server.operators = opers
	server.checkIdent = config.Server.CheckIdent

	// registration
	accountReg := NewAccountRegistration(config.Accounts.Registration)
	server.accountRegistration = &accountReg
	server.channelRegistrationEnabled = config.Channels.Registration.Enabled

	server.configurableStateMutex.Lock()
	server.defaultChannelModes = ParseDefaultChannelModes(config)
	server.configurableStateMutex.Unlock()

	// set new sendqueue size
	if config.Server.MaxSendQBytes != server.MaxSendQBytes {
		server.MaxSendQBytes = config.Server.MaxSendQBytes

		// update on all clients
		server.clients.ByNickMutex.RLock()
		for _, sClient := range server.clients.ByNick {
			sClient.socket.MaxSendQBytes = config.Server.MaxSendQBytes
		}
		server.clients.ByNickMutex.RUnlock()
	}

	// set RPL_ISUPPORT
	oldISupportList := server.isupport
	server.setISupport()
	if oldISupportList != nil {
		newISupportReplies := oldISupportList.GetDifference(server.isupport)
		// push new info to all of our clients
		server.clients.ByNickMutex.RLock()
		for _, sClient := range server.clients.ByNick {
			for _, tokenline := range newISupportReplies {
				// ugly trickery ahead
				sClient.Send(nil, server.name, RPL_ISUPPORT, append([]string{sClient.nick}, tokenline...)...)
			}
		}
		server.clients.ByNickMutex.RUnlock()
	}

	server.loadMOTD(config.Server.MOTD)

	if initial {
		if err := server.loadDatastore(config.Datastore.Path); err != nil {
			return err
		}
	}

	// we are now open for business
	server.setupListeners(config)
	server.setupRestAPI(config)

	return nil
}

func (server *Server) loadMOTD(motdPath string) error {
	server.logger.Debug("rehash", "Loading MOTD")
	motdLines := make([]string, 0)
	if motdPath != "" {
		file, err := os.Open(motdPath)
		if err == nil {
			defer file.Close()

			reader := bufio.NewReader(file)
			for {
				line, err := reader.ReadString('\n')
				if err != nil {
					break
				}
				line = strings.TrimRight(line, "\r\n")
				// "- " is the required prefix for MOTD, we just add it here to make
				// bursting it out to clients easier
				line = fmt.Sprintf("- %s", line)

				motdLines = append(motdLines, line)
			}
		} else {
			return err
		}
	}

	server.configurableStateMutex.Lock()
	defer server.configurableStateMutex.Unlock()
	server.motdLines = motdLines
	return nil
}

func (server *Server) loadDatastore(datastorePath string) error {
	// open the datastore and load server state for which it (rather than config)
	// is the source of truth

	server.logger.Debug("startup", "Opening datastore")
	db, err := OpenDatabase(datastorePath)
	if err == nil {
		server.store = db
	} else {
		return fmt.Errorf("Failed to open datastore: %s", err.Error())
	}

	// load *lines (from the datastores)
	server.logger.Debug("startup", "Loading D/Klines")
	server.loadDLines()
	server.loadKLines()

	// load password manager
	server.logger.Debug("startup", "Loading passwords")
	err = server.store.View(func(tx *buntdb.Tx) error {
		saltString, err := tx.Get(keySalt)
		if err != nil {
			return fmt.Errorf("Could not retrieve salt string: %s", err.Error())
		}

		salt, err := base64.StdEncoding.DecodeString(saltString)
		if err != nil {
			return err
		}

		pwm := NewPasswordManager(salt)
		server.passwords = &pwm
		return nil
	})
	if err != nil {
		return fmt.Errorf("Could not load salt: %s", err.Error())
	}
	return nil
}

func (server *Server) setupListeners(config *Config) {
	// update or destroy all existing listeners
	tlsListeners := config.TLSListeners()
	for addr := range server.listeners {
		currentListener := server.listeners[addr]
		var stillConfigured bool
		for _, newaddr := range config.Server.Listen {
			if newaddr == addr {
				stillConfigured = true
				break
			}
		}

		// pass new config information to the listener, to be picked up after
		// its next Accept(). this is like sending over a buffered channel of
		// size 1, but where sending a second item overwrites the buffered item
		// instead of blocking.
		currentListener.configMutex.Lock()
		currentListener.shouldStop = !stillConfigured
		currentListener.tlsConfig = tlsListeners[addr]
		currentListener.configMutex.Unlock()

		if stillConfigured {
			server.logger.Info("listeners",
				fmt.Sprintf("now listening on %s, tls=%t.", addr, (currentListener.tlsConfig != nil)),
			)
		} else {
			// tell the listener it should stop by interrupting its Accept() call:
			currentListener.listener.Close()
			// TODO(golang1.10) delete stopEvent once issue #21856 is released
			<-currentListener.stopEvent
			delete(server.listeners, addr)
			server.logger.Info("listeners", fmt.Sprintf("stopped listening on %s.", addr))
		}
	}

	// create new listeners that were not previously configured
	for _, newaddr := range config.Server.Listen {
		_, exists := server.listeners[newaddr]
		if !exists {
			// make new listener
			server.listeners[newaddr] = server.createListener(newaddr, tlsListeners[newaddr])
		}
	}

	if len(tlsListeners) == 0 {
		server.logger.Warning("startup", "You are not exposing an SSL/TLS listening port. You should expose at least one port (typically 6697) to accept TLS connections")
	}

	var usesStandardTLSPort bool
	for addr := range config.TLSListeners() {
		if strings.Contains(addr, "6697") {
			usesStandardTLSPort = true
			break
		}
	}
	if 0 < len(tlsListeners) && !usesStandardTLSPort {
		server.logger.Warning("startup", "Port 6697 is the standard TLS port for IRC. You should (also) expose port 6697 as a TLS port to ensure clients can connect securely")
	}
}

func (server *Server) setupRestAPI(config *Config) {
	restAPIEnabled := config.Server.RestAPI.Enabled
	restAPIStarted := server.restAPIServer != nil
	restAPIListenAddrChanged := server.restAPI.Listen != config.Server.RestAPI.Listen

	// stop an existing REST server if it's been disabled or the addr changed
	if restAPIStarted && (!restAPIEnabled || restAPIListenAddrChanged) {
		ctx, _ := context.WithTimeout(context.Background(), httpShutdownTimeout)
		server.restAPIServer.Shutdown(ctx)
		server.restAPIServer.Close()
		server.logger.Info("rehash", "server", fmt.Sprintf("%s rest API stopped on %s.", server.name, server.restAPI.Listen))
		server.restAPIServer = nil
	}

	// start a new one if it's enabled or the addr changed
	if restAPIEnabled && (!restAPIStarted || restAPIListenAddrChanged) {
		server.restAPIServer, _ = StartRestAPI(server, config.Server.RestAPI.Listen)
		server.logger.Info(
			"rehash", "server",
			fmt.Sprintf("%s rest API started on %s.", server.name, config.Server.RestAPI.Listen))
	}

	// save the config information
	server.restAPI = config.Server.RestAPI
}

func (server *Server) GetDefaultChannelModes() Modes {
	server.configurableStateMutex.RLock()
	defer server.configurableStateMutex.RUnlock()
	return server.defaultChannelModes
}

// REHASH
func rehashHandler(server *Server, client *Client, msg ircmsg.IrcMessage) bool {
	server.logger.Info("rehash", fmt.Sprintf("REHASH command used by %s", client.nick))
	err := server.rehash()

	if err == nil {
		client.Send(nil, server.name, RPL_REHASHING, client.nick, "ircd.yaml", "Rehashing")
	} else {
		server.logger.Error("rehash", fmt.Sprintln("Failed to rehash:", err.Error()))
		client.Send(nil, server.name, ERR_UNKNOWNERROR, client.nick, "REHASH", err.Error())
	}
	return false
}

// AWAY [<message>]
func awayHandler(server *Server, client *Client, msg ircmsg.IrcMessage) bool {
	var isAway bool
	var text string
	if len(msg.Params) > 0 {
		isAway = true
		text = msg.Params[0]
		if len(text) > server.limits.AwayLen {
			text = text[:server.limits.AwayLen]
		}
	}

	if isAway {
		client.flags[Away] = true
	} else {
		delete(client.flags, Away)
	}
	client.awayMessage = text

	var op ModeOp
	if client.flags[Away] {
		op = Add
		client.Send(nil, server.name, RPL_NOWAWAY, client.nick, "You have been marked as being away")
	} else {
		op = Remove
		client.Send(nil, server.name, RPL_UNAWAY, client.nick, "You are no longer marked as being away")
	}
	//TODO(dan): Should this be sent automagically as part of setting the flag/mode?
	modech := ModeChanges{ModeChange{
		mode: Away,
		op:   op,
	}}
	client.Send(nil, server.name, "MODE", client.nick, modech.String())

	// dispatch away-notify
	for friend := range client.Friends(caps.AwayNotify) {
		if client.flags[Away] {
			friend.SendFromClient("", "", "", client, nil, "AWAY", client.awayMessage)
		} else {
			friend.SendFromClient("", "", "", client, nil, "AWAY")
		}
	}

	return false
}

// ISON <nick>{ <nick>}
func isonHandler(server *Server, client *Client, msg ircmsg.IrcMessage) bool {
	var nicks = msg.Params

	var err error
	var casefoldedNick string
	ison := make([]string, 0)
	for _, nick := range nicks {
		casefoldedNick, err = CasefoldName(nick)
		if err != nil {
			continue
		}
		if iclient := server.clients.Get(casefoldedNick); iclient != nil {
			ison = append(ison, iclient.nick)
		}
	}

	client.Send(nil, server.name, RPL_ISON, client.nick, strings.Join(nicks, " "))
	return false
}

// MOTD [<target>]
func motdHandler(server *Server, client *Client, msg ircmsg.IrcMessage) bool {
	//TODO(dan): hook this up when we have multiple servers I guess???
	//var target string
	//if len(msg.Params) > 0 {
	//	target = msg.Params[0]
	//}

	server.MOTD(client)
	return false
}

// NOTICE <target>{,<target>} <message>
func noticeHandler(server *Server, client *Client, msg ircmsg.IrcMessage) bool {
	clientOnlyTags := GetClientOnlyTags(msg.Tags)
	targets := strings.Split(msg.Params[0], ",")
	message := msg.Params[1]

	// split privmsg
	splitMsg := server.splitMessage(message, !client.capabilities[caps.MaxLine])

	// get label and apply to response
	var label string
	labelValue, exists := msg.Tags["draft/label"]
	if exists && labelValue.Value != "" {
		label = labelValue.Value
	}

	for i, targetString := range targets {
		// max of four targets per privmsg
		if i > maxTargets-1 {
			break
		}
		prefixes, targetString := SplitChannelMembershipPrefixes(targetString)
		lowestPrefix := GetLowestChannelModePrefix(prefixes)

		target, cerr := CasefoldChannel(targetString)
		if cerr == nil {
			channel := server.channels.Get(target)
			if channel == nil {
				// errors silently ignored with NOTICE as per RFC
				continue
			}
			if !channel.CanSpeak(client) {
				// errors silently ignored with NOTICE as per RFC
				continue
			}
			msgid := server.generateMessageID()
			channel.SplitNotice(msgid, label, lowestPrefix, clientOnlyTags, client, splitMsg)
		} else {
			target, err := CasefoldName(targetString)
			if err != nil {
				continue
			}
			if target == "chanserv" {
				server.chanservReceiveNotice(client, message)
				continue
			} else if target == "nickserv" {
				server.nickservReceiveNotice(client, message)
				continue
			}

			user := server.clients.Get(target)
			if user == nil {
				// errors silently ignored with NOTICE as per RFC
				continue
			}
			if !user.capabilities[caps.MessageTags] {
				clientOnlyTags = nil
			}
			msgid := server.generateMessageID()
			// restrict messages appropriately when +R is set
			// intentionally make the sending user think the message went through fine
			if !user.flags[RegisteredOnly] || client.registered {
				user.SendSplitMsgFromClient(msgid, "", client, clientOnlyTags, "NOTICE", user.nick, splitMsg)
			}

			if client.capabilities[caps.EchoMessage] {
				client.SendSplitMsgFromClient(msgid, label, client, clientOnlyTags, "NOTICE", user.nick, splitMsg)
			}
		}
	}
	return false
}

// KICK <channel>{,<channel>} <user>{,<user>} [<comment>]
func kickHandler(server *Server, client *Client, msg ircmsg.IrcMessage) bool {
	channels := strings.Split(msg.Params[0], ",")
	users := strings.Split(msg.Params[1], ",")
	if (len(channels) != len(users)) && (len(users) != 1) {
		client.Send(nil, server.name, ERR_NEEDMOREPARAMS, client.nick, "KICK", "Not enough parameters")
		return false
	}

	var kicks [][]string
	for index, channel := range channels {
		if len(users) == 1 {
			kicks = append(kicks, []string{channel, users[0]})
		} else {
			kicks = append(kicks, []string{channel, users[index]})
		}
	}

	var comment string
	if len(msg.Params) > 2 {
		comment = msg.Params[2]
	}
	for _, info := range kicks {
		chname := info[0]
		nickname := info[1]
		casefoldedChname, err := CasefoldChannel(chname)
		channel := server.channels.Get(casefoldedChname)
		if err != nil || channel == nil {
			client.Send(nil, server.name, ERR_NOSUCHCHANNEL, client.nick, chname, "No such channel")
			continue
		}

		casefoldedNickname, err := CasefoldName(nickname)
		target := server.clients.Get(casefoldedNickname)
		if err != nil || target == nil {
			client.Send(nil, server.name, ERR_NOSUCHNICK, client.nick, nickname, "No such nick")
			continue
		}

		// make sure client has privs to kick the given user
		//TODO(dan): split this into a separate function that checks if users have privs
		// over other users, useful for things like -aoh as well
		channel.membersMutex.Lock()

		var hasPrivs bool
		for _, mode := range ChannelPrivModes {
			if channel.members[client][mode] {
				hasPrivs = true

				// admins cannot kick other admins
				if mode == ChannelAdmin && channel.members[target][ChannelAdmin] {
					hasPrivs = false
				}

				break
			} else if channel.members[target][mode] {
				break
			}
		}

		if hasPrivs {
			if comment == "" {
				comment = nickname
			}
			channel.kickNoMutex(client, target, comment)
		} else {
			client.Send(nil, client.server.name, ERR_CHANOPRIVSNEEDED, chname, "You're not a channel operator")
		}

		channel.membersMutex.Unlock()
	}
	return false
}

// elistMatcher takes and matches ELIST conditions
type elistMatcher struct {
	MinClientsActive bool
	MinClients       int
	MaxClientsActive bool
	MaxClients       int
}

// Matches checks whether the given channel matches our matches.
func (matcher *elistMatcher) Matches(channel *Channel) bool {
	channel.membersMutex.RLock()
	defer channel.membersMutex.RUnlock()

	if matcher.MinClientsActive {
		if len(channel.members) < matcher.MinClients {
			return false
		}
	}

	if matcher.MaxClientsActive {
		if matcher.MaxClients < len(channel.members) {
			return false
		}
	}

	return true
}

// LIST [<channel>{,<channel>}] [<elistcond>{,<elistcond>}]
func listHandler(server *Server, client *Client, msg ircmsg.IrcMessage) bool {
	// get channels
	var channels []string
	for _, param := range msg.Params {
		if 0 < len(param) && param[0] == '#' {
			for _, channame := range strings.Split(param, ",") {
				if 0 < len(channame) && channame[0] == '#' {
					channels = append(channels, channame)
				}
			}
		}
	}

	// get elist conditions
	var matcher elistMatcher
	for _, param := range msg.Params {
		if len(param) < 1 {
			continue
		}

		if param[0] == '<' {
			param = param[1:]
			val, err := strconv.Atoi(param)
			if err != nil {
				continue
			}
			matcher.MaxClientsActive = true
			matcher.MaxClients = val - 1 // -1 because < means less than the given number
		}
		if param[0] == '>' {
			param = param[1:]
			val, err := strconv.Atoi(param)
			if err != nil {
				continue
			}
			matcher.MinClientsActive = true
			matcher.MinClients = val + 1 // +1 because > means more than the given number
		}
	}

	if len(channels) == 0 {
		server.channels.ChansLock.RLock()
		for _, channel := range server.channels.Chans {
			if !client.flags[Operator] && channel.flags[Secret] {
				continue
			}
			if matcher.Matches(channel) {
				client.RplList(channel)
			}
		}
		server.channels.ChansLock.RUnlock()
	} else {
		// limit regular users to only listing one channel
		if !client.flags[Operator] {
			channels = channels[:1]
		}

		for _, chname := range channels {
			casefoldedChname, err := CasefoldChannel(chname)
			channel := server.channels.Get(casefoldedChname)
			if err != nil || channel == nil || (!client.flags[Operator] && channel.flags[Secret]) {
				if len(chname) > 0 {
					client.Send(nil, server.name, ERR_NOSUCHCHANNEL, client.nick, chname, "No such channel")
				}
				continue
			}
			if matcher.Matches(channel) {
				client.RplList(channel)
			}
		}
	}
	client.Send(nil, server.name, RPL_LISTEND, client.nick, "End of LIST")
	return false
}

// RplList returns the RPL_LIST numeric for the given channel.
func (target *Client) RplList(channel *Channel) {
	channel.membersMutex.RLock()
	defer channel.membersMutex.RUnlock()

	// get the correct number of channel members
	var memberCount int
	if target.flags[Operator] || channel.members.Has(target) {
		memberCount = len(channel.members)
	} else {
		for member := range channel.members {
			if !member.flags[Invisible] {
				memberCount++
			}
		}
	}

	target.Send(nil, target.server.name, RPL_LIST, target.nick, channel.name, strconv.Itoa(memberCount), channel.topic)
}

// NAMES [<channel>{,<channel>}]
func namesHandler(server *Server, client *Client, msg ircmsg.IrcMessage) bool {
	var channels []string
	if len(msg.Params) > 0 {
		channels = strings.Split(msg.Params[0], ",")
	}
	//var target string
	//if len(msg.Params) > 1 {
	//	target = msg.Params[1]
	//}

	if len(channels) == 0 {
		server.channels.ChansLock.RLock()
		for _, channel := range server.channels.Chans {
			channel.Names(client)
		}
		server.channels.ChansLock.RUnlock()
		return false
	}

	// limit regular users to only listing one channel
	if !client.flags[Operator] {
		channels = channels[:1]
	}

	for _, chname := range channels {
		casefoldedChname, err := CasefoldChannel(chname)
		channel := server.channels.Get(casefoldedChname)
		if err != nil || channel == nil {
			if len(chname) > 0 {
				client.Send(nil, server.name, ERR_NOSUCHCHANNEL, client.nick, chname, "No such channel")
			}
			continue
		}
		channel.Names(client)
	}
	return false
}

// VERSION [<server>]
func versionHandler(server *Server, client *Client, msg ircmsg.IrcMessage) bool {
	var target string
	if len(msg.Params) > 0 {
		target = msg.Params[0]
	}
	casefoldedTarget, err := Casefold(target)
	if target != "" && (err != nil || casefoldedTarget != server.nameCasefolded) {
		client.Send(nil, server.name, ERR_NOSUCHSERVER, client.nick, target, "No such server")
		return false
	}

	client.Send(nil, server.name, RPL_VERSION, client.nick, Ver, server.name)
	client.RplISupport()
	return false
}

// INVITE <nickname> <channel>
func inviteHandler(server *Server, client *Client, msg ircmsg.IrcMessage) bool {
	nickname := msg.Params[0]
	channelName := msg.Params[1]

	casefoldedNickname, err := CasefoldName(nickname)
	target := server.clients.Get(casefoldedNickname)
	if err != nil || target == nil {
		client.Send(nil, server.name, ERR_NOSUCHNICK, client.nick, nickname, "No such nick")
		return false
	}

	casefoldedChannelName, err := CasefoldChannel(channelName)
	channel := server.channels.Get(casefoldedChannelName)
	if err != nil || channel == nil {
		client.Send(nil, server.name, RPL_INVITING, client.nick, target.nick, channelName)
		target.Send(nil, client.nickMaskString, "INVITE", target.nick, channel.name)
		return true
	}

	channel.Invite(target, client)
	return false
}

// TIME [<server>]
func timeHandler(server *Server, client *Client, msg ircmsg.IrcMessage) bool {
	var target string
	if len(msg.Params) > 0 {
		target = msg.Params[0]
	}
	casefoldedTarget, err := Casefold(target)
	if (target != "") && err != nil || (casefoldedTarget != server.nameCasefolded) {
		client.Send(nil, server.name, ERR_NOSUCHSERVER, client.nick, target, "No such server")
		return false
	}
	client.Send(nil, server.name, RPL_TIME, client.nick, server.name, time.Now().Format(time.RFC1123))
	return false
}

// KILL <nickname> <comment>
func killHandler(server *Server, client *Client, msg ircmsg.IrcMessage) bool {
	nickname := msg.Params[0]
	comment := "<no reason supplied>"
	if len(msg.Params) > 1 {
		comment = msg.Params[1]
	}

	casefoldedNickname, err := CasefoldName(nickname)
	target := server.clients.Get(casefoldedNickname)
	if err != nil || target == nil {
		client.Send(nil, client.server.name, ERR_NOSUCHNICK, client.nick, nickname, "No such nick")
		return false
	}

	quitMsg := fmt.Sprintf("Killed (%s (%s))", client.nick, comment)

	server.snomasks.Send(sno.LocalKills, fmt.Sprintf(ircfmt.Unescape("%s$r was killed by %s $c[grey][$r%s$c[grey]]"), target.nick, client.nick, comment))
	target.exitedSnomaskSent = true

	target.Quit(quitMsg)
	target.destroy()
	return false
}

// WHOWAS <nickname> [<count> [<server>]]
func whowasHandler(server *Server, client *Client, msg ircmsg.IrcMessage) bool {
	nicknames := strings.Split(msg.Params[0], ",")

	var count int64
	if len(msg.Params) > 1 {
		count, _ = strconv.ParseInt(msg.Params[1], 10, 64)
	}
	//var target string
	//if len(msg.Params) > 2 {
	//	target = msg.Params[2]
	//}
	for _, nickname := range nicknames {
		results := server.whoWas.Find(nickname, count)
		if len(results) == 0 {
			if len(nickname) > 0 {
				client.Send(nil, server.name, ERR_WASNOSUCHNICK, client.nick, nickname, "There was no such nickname")
			}
		} else {
			for _, whoWas := range results {
				client.Send(nil, server.name, RPL_WHOWASUSER, client.nick, whoWas.nickname, whoWas.username, whoWas.hostname, "*", whoWas.realname)
			}
		}
		if len(nickname) > 0 {
			client.Send(nil, server.name, RPL_ENDOFWHOWAS, client.nick, nickname, "End of WHOWAS")
		}
	}
	return false
}

// LUSERS [<mask> [<server>]]
func lusersHandler(server *Server, client *Client, msg ircmsg.IrcMessage) bool {
	//TODO(vegax87) Fix network statistics and additional parameters
	var totalcount, invisiblecount, opercount int

	server.clients.ByNickMutex.RLock()
	defer server.clients.ByNickMutex.RUnlock()

	for _, onlineusers := range server.clients.ByNick {
		totalcount++
		if onlineusers.flags[Invisible] {
			invisiblecount++
		}
		if onlineusers.flags[Operator] {
			opercount++
		}
	}
	client.Send(nil, server.name, RPL_LUSERCLIENT, client.nick, fmt.Sprintf("There are %d users and %d invisible on %d server(s)", totalcount, invisiblecount, 1))
	client.Send(nil, server.name, RPL_LUSEROP, client.nick, fmt.Sprintf("%d IRC Operators online", opercount))
	client.Send(nil, server.name, RPL_LUSERCHANNELS, client.nick, fmt.Sprintf("%d channels formed", server.channels.Len()))
	client.Send(nil, server.name, RPL_LUSERME, client.nick, fmt.Sprintf("I have %d clients and %d servers", totalcount, 1))
	return false
}

// USERHOST <nickname> [<nickname> <nickname> ...]
func userhostHandler(server *Server, client *Client, msg ircmsg.IrcMessage) bool {
	returnedNicks := make(map[string]bool)

	for i, nickname := range msg.Params {
		if i >= 10 {
			break
		}

		casefoldedNickname, err := CasefoldName(nickname)
		target := server.clients.Get(casefoldedNickname)
		if err != nil || target == nil {
			client.Send(nil, client.server.name, ERR_NOSUCHNICK, client.nick, nickname, "No such nick")
			return false
		}
		if returnedNicks[casefoldedNickname] {
			continue
		}

		// to prevent returning multiple results for a single nick
		returnedNicks[casefoldedNickname] = true

		var isOper, isAway string

		if target.flags[Operator] {
			isOper = "*"
		}
		if target.flags[Away] {
			isAway = "-"
		} else {
			isAway = "+"
		}
		client.Send(nil, client.server.name, RPL_USERHOST, client.nick, fmt.Sprintf("%s%s=%s%s@%s", target.nick, isOper, isAway, target.username, target.hostname))
	}

	return false
}

// PROXY TCP4/6 SOURCEIP DESTIP SOURCEPORT DESTPORT
// http://www.haproxy.org/download/1.8/doc/proxy-protocol.txt
func proxyHandler(server *Server, client *Client, msg ircmsg.IrcMessage) bool {
	// only allow unregistered clients to use this command
	if client.registered {
		return false
	}

	clientAddress := IPString(client.socket.conn.RemoteAddr())
	clientHostname := client.hostname
	for _, address := range server.proxyAllowedFrom {
		if clientHostname == address || clientAddress == address {
			proxiedIP := msg.Params[1]

			// ensure IP is sane
			parsedProxiedIP := net.ParseIP(proxiedIP)
			if parsedProxiedIP == nil {
				client.Quit(fmt.Sprintf("Proxied IP address is not valid: [%s]", proxiedIP))
				return true
			}

			isBanned, banMsg := server.checkBans(parsedProxiedIP)
			if isBanned {
				client.Quit(banMsg)
				return true
			}

			// override the client's regular IP
			client.proxiedIP = msg.Params[1]
			client.rawHostname = LookupHostname(msg.Params[1])
			client.hostname = client.rawHostname
			return false
		}
	}
	client.Quit("PROXY command is not usable from your address")
	return true
}
