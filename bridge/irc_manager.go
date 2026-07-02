package bridge

import (
        "fmt"
        "regexp"
        "strings"
        "time"

        "github.com/mozillazg/go-unidecode"
        "github.com/pkg/errors"
        ircnick "github.com/qaisjp/go-discord-irc/irc/nick"
        "github.com/qaisjp/go-discord-irc/irc/varys"
        irc "github.com/qaisjp/go-ircevent"
        log "github.com/sirupsen/logrus"
)

// DevMode is a hack
var DevMode = false

// Color map and palette
var ircColors = []int{2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14} // red, green, yellow, etc.
var userColorMap = make(map[string]int) // DiscordID -> IRC color

// Helper function for nickname coloring
func getColoredNick(discordID, nick string) string {
	color, ok := userColorMap[discordID]
   	if !ok {
       		color = ircColors[len(userColorMap)%len(ircColors)]
       		userColorMap[discordID] = color
   	}
   	return fmt.Sprintf("\x03%02d%s\x03", color, nick) // \x03 is IRC color code
}

// IRCManager should only be used from one thread.
type IRCManager struct {
        ircConnections map[string]*ircConnection
        puppetNicks    map[string]*ircConnection
        bridge         *Bridge
        varys          varys.Client
}

// NewIRCManager creates a new IRCManager
func newIRCManager(bridge *Bridge) (*IRCManager, error) {
        conf := bridge.Config
        m := &IRCManager{
                ircConnections: make(map[string]*ircConnection),
                puppetNicks:    make(map[string]*ircConnection),
                bridge:         bridge,
        }
        m.varys = varys.NewMemClient()
        err := m.varys.Setup(varys.SetupParams{
                UseTLS:             !conf.NoTLS,
                InsecureSkipVerify: conf.InsecureSkipVerify,
                Server:             conf.IRCServer,
                ServerPassword:     conf.IRCServerPass,
                WebIRCPassword:     conf.WebIRCPass,
        })
        if err != nil {
                return nil, fmt.Errorf("failed to set up params: %w", err)
        }

        discordToNicks, err := m.varys.GetUIDToNicks()
        if err != nil {
                return nil, fmt.Errorf("failed to get discordToNicks: %w", err)
        }

        m.ircConnections = make(map[string]*ircConnection, len(discordToNicks))
        m.puppetNicks = make(map[string]*ircConnection, len(discordToNicks))
        for discord, nick := range discordToNicks {
                m.ircConnections[discord] = &ircConnection{
                        discord:          DiscordUser{ID: discord},
                        nick:             nick,
                        messages:         make(chan IRCMessage),
                        manager:          m,
                        pmNoticedSenders: make(map[string]struct{}),
                }
        }
        return m, nil
}

// CloseConnection shuts down a particular connection and its channels.
func (m *IRCManager) CloseConnection(i *ircConnection) {
        log.WithField("nick", i.nick).Println("Closing connection.")
        if i.cooldownTimer != nil {
                i.cooldownTimer.Stop()
                i.cooldownTimer = nil
        }
        delete(m.ircConnections, i.discord.ID)
        delete(m.puppetNicks, i.nick)
        close(i.messages)
        if DevMode {
                fmt.Println("Decrementing total connections. It's now", len(m.ircConnections))
        }
        if err := m.varys.QuitIfConnected(i.discord.ID, i.quitMessage); err != nil {
                log.WithError(err).WithFields(log.Fields{"discord": i.discord.ID}).Errorln("failed to quit")
        }
}

// Close closes all of an IRCManager's connections.
func (m *IRCManager) Close() {
        for _, con := range m.ircConnections {
                m.CloseConnection(con)
        }
}

// SetConnectionCooldown renews/starts a timer for expiring a connection.
func (m *IRCManager) SetConnectionCooldown(con *ircConnection) {
        if con.cooldownTimer != nil {
                log.WithField("nick", con.nick).Println("IRC connection cooldownTimer stopped!")
                con.cooldownTimer.Stop()
        }
        con.cooldownTimer = time.AfterFunc(
                m.bridge.Config.CooldownDuration,
                func() {
                        log.WithField("nick", con.nick).Println("IRC connection expired by cooldownTimer...")
                        m.CloseConnection(con)
                },
        )
        log.WithField("nick", con.nick).Println("IRC connection cooldownTimer created...")
}

// DisconnectUser immediately disconnects a Discord user if it exists
func (m *IRCManager) DisconnectUser(userID string) {
        con, ok := m.ircConnections[userID]
        if !ok {
                return
        }
        m.CloseConnection(con)
}

var connectionsIgnored = 0

func (m *IRCManager) ircIgnoredDiscord(user string) bool {
        _, ret := m.bridge.Config.DiscordIgnores[user]
        return ret
}

// HandleUser deals with messages sent from a DiscordUser
func (m *IRCManager) HandleUser(user DiscordUser) {
        if m.ircIgnoredDiscord(user.ID) {
                return
        }
        if allowed := m.bridge.Config.DiscordAllowed; allowed != nil {
                if _, ok := allowed[user.ID]; !ok {
                        return
                }
        }
        if con, ok := m.ircConnections[user.ID]; ok {
                if !user.Online {
                        m.SetConnectionCooldown(con)
                        con.SetAway("offline on discord")
                } else {
                        if con.cooldownTimer != nil {
                                log.WithField("nick", user.Nick).Println("Destroying connection cooldown.")
                                con.cooldownTimer.Stop()
                                con.cooldownTimer = nil
                                con.SetAway("")
                        }
                }
                if user.Nick == "" {
                        return
                }
                con.UpdateDetails(user)
                return
        }
        if user.Username == "" || user.Discriminator == "" {
                if !user.Online {
                        return
                }
                log.WithFields(log.Fields{
                        "err":                errors.WithStack(errors.New("Username or Discriminator is empty")).Error(),
                        "user.Username":      user.Username,
                        "user.Discriminator": user.Discriminator,
                        "user.ID":            user.ID,
                }).Println("ignoring a HandleUser (in irc_manager.go)")
                return
        }
        if DevMode {
                if len(m.ircConnections) > 4 && !strings.Contains(user.Username, "qais") {
                        connectionsIgnored++
                        return
                }
        }
        if m.bridge.Config.ConnectionLimit > 0 && len(m.ircConnections)+1 >= m.bridge.Config.ConnectionLimit {
                return
        }
        nick := m.generateNickname(user)
        username := m.generateUsername(user)

        var ip string
        {
                baseip := "fd75:f5f5:226f:"
                if user.Bot {
                        baseip += "2"
                } else {
                        baseip += "1"
                }
                ip = SnowflakeToIP(baseip, user.ID)
        }

        hostname := user.ID
        if user.Bot {
                hostname += ".bot.discord"
        } else {
                hostname += ".user.discord"
        }

        con := &ircConnection{
                discord:          user,
                nick:             nick,
                messages:         make(chan IRCMessage),
                manager:          m,
                pmNoticedSenders: make(map[string]struct{}),
                quitMessage:      fmt.Sprintf("Offline for %s", m.bridge.Config.CooldownDuration),
        }
        m.ircConnections[user.ID] = con
        m.puppetNicks[nick] = con

        if DevMode {
                fmt.Println("Incrementing total connections. It's now", len(m.ircConnections))
        }

        err := m.varys.Connect(varys.ConnectParams{
                UID:          user.ID,
                Nick:         nick,
                Username:     username,
                RealName:     user.Username,
                WebIRCSuffix: fmt.Sprintf("discord %s %s", hostname, ip),
                Callbacks: map[string]func(*irc.Event){
                        "001":     con.OnWelcome,
                        "PRIVMSG": con.OnPrivateMessage,
                },
        })
        if err != nil {
                log.WithError(err).Errorln("error opening irc connection")
                return
        }
}

// sanitiseNickname remains unchanged
func sanitiseNickname(nick string) string {
        if nick == "" {
                fmt.Println(errors.WithStack(errors.New("trying to sanitise an empty nick")))
                return "_"
        }
        if newnick := unidecode.Unidecode(nick); newnick != "" {
                nick = newnick
        }
        if nick[0] == '-' {
                nick = "_" + nick
        }
        if ircnick.IsDigit(nick[0]) {
                nick = "_" + nick
        }
        newNick := []byte(nick)
        for i, c := range []byte(nick) {
                if !ircnick.IsNickChar(c) || ircnick.IsFakeNickChar(c) {
                        newNick[i] = ' '
                }
        }
        newNick = regexp.MustCompile(` +`).ReplaceAllLiteral(newNick, []byte{'_'})
        return string(newNick)
}

func (m *IRCManager) generateNickname(discord DiscordUser) string {
        nick := sanitiseNickname(discord.Nick)
        suffix := m.bridge.Config.Suffix
        newNick := nick + suffix
        useFallback := len(newNick) > m.bridge.Config.MaxNickLength || m.bridge.ircListener.DoesUserExist(newNick)
        if !useFallback {
                guild, err := m.bridge.discord.Session.State.Guild(m.bridge.Config.GuildID)
                if err != nil {
                        return ""
                }
                for _, member := range guild.Members {
                        if member.User.ID == discord.ID {
                                continue
                        }
                        name := member.Nick
                        if member.Nick == "" {
                                name = member.User.Username
                        }
                        if name == "" {
                                log.WithField("member", member).Errorln("blank username encountered")
                                continue
                        }
                        if strings.EqualFold(sanitiseNickname(name), nick) {
                                useFallback = true
                                break
                        }
                }
        }
        if useFallback {
                discriminator := discord.Discriminator
                username := sanitiseNickname(discord.Username)
                suffix = m.bridge.Config.Separator + discriminator + suffix
                length := ircnick.MAXLENGTH - len(suffix)
                if length >= len(username) {
                        length = len(username)
                }
                return username[:length] + suffix
        }
        return newNick
}

// SendMessage sends Discord messages to IRC with optional nickname use
func (m *IRCManager) SendMessage(channel string, msg *DiscordMessage) {
        if m.ircIgnoredDiscord(msg.Author.ID) {
                return
        }
        con, ok := m.ircConnections[msg.Author.ID]
        content := msg.Content
        channel = strings.Split(channel, " ")[0]

        // Get Discord nickname if possible
        nick := msg.Author.Username
        member, err := m.bridge.discord.Session.GuildMember(m.bridge.Config.GuildID, msg.Author.ID)
        if err == nil && member.Nick != "" {
                nick = member.Nick
        }

        // Generate color for Discord nick if needed
        coloredNick := getColoredNick(msg.Author.ID, nick)

        // Simple mode or offline users (no IRC puppet)
        if m.bridge.Config.SimpleMode || !ok {
                for _, line := range strings.Split(content, "\n") {
                        var msgToSend string

                        // In Simple Mode: never show any Discord username/nick
                        if m.bridge.Config.SimpleMode {
                                msgToSend = line
                        } else {
                                // If puppet connection missing due to connection limit, show colored nick
                                msgToSend = fmt.Sprintf("%s %s", coloredNick, line)
                        }

                        m.bridge.ircListener.Privmsg(channel, msgToSend)
                }
                return
        }

        // Puppet exists (full mode)
        if con.cooldownTimer != nil {
                m.SetConnectionCooldown(con)
        }

        for _, line := range strings.Split(content, "\n") {
                ircMessage := IRCMessage{
                        IRCChannel: channel,
                        Message:    line,
                        IsAction:   msg.IsAction,
                }

                if strings.HasPrefix(line, "/me ") && len(line) > 4 {
                        ircMessage.IsAction = true
                        ircMessage.Message = line[4:]
                }

                if m.isFilteredDiscordMessage(line) {
                        continue
                }

                select {
                case con.messages <- ircMessage:
                case <-time.After(time.Millisecond * 5):
                        go func() { con.messages <- ircMessage }()
                }
        }
}

func (m *IRCManager) RequestChannels(userID string) []Mapping {
        return m.bridge.mappings
}

func (m *IRCManager) isIgnoredHostmask(mask string) bool {
        for _, ban := range m.bridge.Config.IRCIgnores {
                if ban.Match(mask) {
                        return true
                }
        }
        return false
}

func (m *IRCManager) isFilteredIRCMessage(txt string) bool {
        for _, ban := range m.bridge.Config.IRCFilteredMessages {
                if ban.Match(txt) {
                        return true
                }
        }
        return false
}

func (m *IRCManager) isFilteredDiscordMessage(txt string) bool {
        for _, ban := range m.bridge.Config.DiscordFilteredMessages {
                if ban.Match(txt) {
                        return true
                }
        }
        return false
}

func (m *IRCManager) generateUsername(discordUser DiscordUser) string {
        if len(m.bridge.Config.PuppetUsername) > 0 {
                return m.bridge.Config.PuppetUsername
        }
        return sanitiseNickname(discordUser.Username)
}

// FindConnectionByDiscordID returns an IRC connection if a puppet exists for the given Discord user ID.
func (m *IRCManager) FindConnectionByDiscordID(id string) *ircConnection {
        for _, conn := range m.ircConnections {
                if conn.discord.ID == id {
                        return conn
                }
        }
        return nil
}
