// Copyright (c) 2015-present Mattermost, Inc. All Rights Reserved.
// See License.txt for license information.

package app

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"mime/multipart"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"net/http"

	l4g "github.com/alecthomas/log4go"
	"github.com/mattermost/mattermost-server/model"
	"github.com/mattermost/mattermost-server/utils"
)

type SlackChannel struct {
	Id      string            `json:"id"`
	Name    string            `json:"name"`
	Members []string          `json:"members"`
	Topic   map[string]string `json:"topic"`
	Purpose map[string]string `json:"purpose"`
}

type SlackUser struct {
	Id       string            `json:"id"`
	Username string            `json:"name"`
	Profile  map[string]string `json:"profile"`
}

type SlackFile struct {
	Id    string `json:"id"`
	Title string `json:"title"`
}

type SlackPost struct {
	User        string                   `json:"user"`
	BotId       string                   `json:"bot_id"`
	BotUsername string                   `json:"username"`
	Text        string                   `json:"text"`
	TimeStamp   string                   `json:"ts"`
	Type        string                   `json:"type"`
	SubType     string                   `json:"subtype"`
	Comment     *SlackComment            `json:"comment"`
	Upload      bool                     `json:"upload"`
	File        *SlackFile               `json:"file"`
	Attachments []*model.SlackAttachment `json:"attachments"`
}

var isValidChannelNameCharacters = regexp.MustCompile(`^[a-zA-Z0-9\-_]+$`).MatchString

type SlackComment struct {
	User    string `json:"user"`
	Comment string `json:"comment"`
}

func truncateRunes(s string, i int) string {
	runes := []rune(s)
	if len(runes) > i {
		return string(runes[:i])
	}
	return s
}

func SlackConvertTimeStamp(ts string) int64 {
	timeString := strings.SplitN(ts, ".", 2)[0]

	timeStamp, err := strconv.ParseInt(timeString, 10, 64)
	if err != nil {
		l4g.Warn(utils.T("api.slackimport.slack_convert_timestamp.bad.warn"))
		return 1
	}
	return timeStamp * 1000 // Convert to milliseconds
}

func SlackConvertChannelName(channelName string, channelId string) string {
	newName := strings.Trim(channelName, "_-")
	if len(newName) == 1 {
		return "slack-channel-" + newName
	}

	if isValidChannelNameCharacters(newName) {
		return newName
	} else {
		return strings.ToLower(channelId)
	}
}

func SlackParseChannels(data io.Reader) ([]SlackChannel, error) {
	decoder := json.NewDecoder(data)

	var channels []SlackChannel
	if err := decoder.Decode(&channels); err != nil {
		l4g.Warn(utils.T("api.slackimport.slack_parse_channels.error"))
		return channels, err
	}
	return channels, nil
}

func SlackParseUsers(data io.Reader) ([]SlackUser, error) {
	decoder := json.NewDecoder(data)

	var users []SlackUser
	if err := decoder.Decode(&users); err != nil {
		// This actually returns errors that are ignored.
		// In this case it is erroring because of a null that Slack
		// introduced. So we just return the users here.
		return users, err
	}
	return users, nil
}

func SlackParsePosts(data io.Reader) ([]SlackPost, error) {
	decoder := json.NewDecoder(data)

	var posts []SlackPost
	if err := decoder.Decode(&posts); err != nil {
		l4g.Warn(utils.T("api.slackimport.slack_parse_posts.error"))
		return posts, err
	}
	return posts, nil
}

func (a *App) SlackAddUsers(teamId string, slackusers []SlackUser, log *bytes.Buffer) map[string]*model.User {
	// Log header
	log.WriteString(utils.T("api.slackimport.slack_add_users.created"))
	log.WriteString("===============\r\n\r\n")

	addedUsers := make(map[string]*model.User)

	// Need the team
	var team *model.Team
	if result := <-a.Srv.Store.Team().Get(teamId); result.Err != nil {
		log.WriteString(utils.T("api.slackimport.slack_import.team_fail"))
		return addedUsers
	} else {
		team = result.Data.(*model.Team)
	}

	for _, sUser := range slackusers {
		firstName := ""
		lastName := ""
		if name, ok := sUser.Profile["first_name"]; ok {
			firstName = name
		}
		if name, ok := sUser.Profile["last_name"]; ok {
			lastName = name
		}

		email := sUser.Profile["email"]
		if email == "" {
			email = sUser.Username + "@example.com"
			log.WriteString(utils.T("api.slackimport.slack_add_users.missing_email_address", map[string]interface{}{"Email": email, "Username": sUser.Username}))
			l4g.Warn(utils.T("api.slackimport.slack_add_users.missing_email_address.warn", map[string]interface{}{"Email": email, "Username": sUser.Username}))
		}

		password := model.NewId()

		// Check for email conflict and use existing user if found
		if result := <-a.Srv.Store.User().GetByEmail(email); result.Err == nil {
			existingUser := result.Data.(*model.User)
			addedUsers[sUser.Id] = existingUser
			if err := a.JoinUserToTeam(team, addedUsers[sUser.Id], ""); err != nil {
				log.WriteString(utils.T("api.slackimport.slack_add_users.merge_existing_failed", map[string]interface{}{"Email": existingUser.Email, "Username": existingUser.Username}))
			} else {
				log.WriteString(utils.T("api.slackimport.slack_add_users.merge_existing", map[string]interface{}{"Email": existingUser.Email, "Username": existingUser.Username}))
			}
			continue
		}

		newUser := model.User{
			Username:  sUser.Username,
			FirstName: firstName,
			LastName:  lastName,
			Email:     email,
			Password:  password,
		}

		if mUser := a.OldImportUser(team, &newUser); mUser != nil {
			addedUsers[sUser.Id] = mUser
			log.WriteString(utils.T("api.slackimport.slack_add_users.email_pwd", map[string]interface{}{"Email": newUser.Email, "Password": password}))
		} else {
			log.WriteString(utils.T("api.slackimport.slack_add_users.unable_import", map[string]interface{}{"Username": sUser.Username}))
		}
	}

	return addedUsers
}

func (a *App) SlackAddBotUser(teamId string, log *bytes.Buffer) *model.User {
	var team *model.Team
	if result := <-a.Srv.Store.Team().Get(teamId); result.Err != nil {
		log.WriteString(utils.T("api.slackimport.slack_import.team_fail"))
		return nil
	} else {
		team = result.Data.(*model.Team)
	}

	password := model.NewId()
	username := "slackimportuser_" + model.NewId()
	email := username + "@localhost"

	botUser := model.User{
		Username:  username,
		FirstName: "",
		LastName:  "",
		Email:     email,
		Password:  password,
	}

	if mUser := a.OldImportUser(team, &botUser); mUser != nil {
		log.WriteString(utils.T("api.slackimport.slack_add_bot_user.email_pwd", map[string]interface{}{"Email": botUser.Email, "Password": password}))
		return mUser
	} else {
		log.WriteString(utils.T("api.slackimport.slack_add_bot_user.unable_import", map[string]interface{}{"Username": username}))
		return nil
	}
}

func (a *App) SlackAddPosts(teamId string, channel *model.Channel, posts []SlackPost, users map[string]*model.User, uploads map[string]*zip.File, botUser *model.User) {
	for _, sPost := range posts {
		switch {
		case sPost.Type == "message" && (sPost.SubType == "" || sPost.SubType == "file_share"):
			if sPost.User == "" {
				l4g.Debug(utils.T("api.slackimport.slack_add_posts.without_user.debug"))
				continue
			} else if users[sPost.User] == nil {
				l4g.Debug(utils.T("api.slackimport.slack_add_posts.user_no_exists.debug"), sPost.User)
				continue
			}
			newPost := model.Post{
				UserId:    users[sPost.User].Id,
				ChannelId: channel.Id,
				Message:   sPost.Text,
				CreateAt:  SlackConvertTimeStamp(sPost.TimeStamp),
			}
			if sPost.Upload {
				if fileInfo, ok := a.SlackUploadFile(sPost, uploads, teamId, newPost.ChannelId, newPost.UserId); ok {
					newPost.FileIds = append(newPost.FileIds, fileInfo.Id)
					newPost.Message = sPost.File.Title
				}
			}
			a.OldImportPost(&newPost)
			for _, fileId := range newPost.FileIds {
				if result := <-a.Srv.Store.FileInfo().AttachToPost(fileId, newPost.Id); result.Err != nil {
					l4g.Error(utils.T("api.slackimport.slack_add_posts.attach_files.error"), newPost.Id, newPost.FileIds, result.Err)
				}
			}

		case sPost.Type == "message" && sPost.SubType == "file_comment":
			if sPost.Comment == nil {
				l4g.Debug(utils.T("api.slackimport.slack_add_posts.msg_no_comment.debug"))
				continue
			} else if sPost.Comment.User == "" {
				l4g.Debug(utils.T("api.slackimport.slack_add_posts.msg_no_usr.debug"))
				continue
			} else if users[sPost.Comment.User] == nil {
				l4g.Debug(utils.T("api.slackimport.slack_add_posts.user_no_exists.debug"), sPost.User)
				continue
			}
			newPost := model.Post{
				UserId:    users[sPost.Comment.User].Id,
				ChannelId: channel.Id,
				Message:   sPost.Comment.Comment,
				CreateAt:  SlackConvertTimeStamp(sPost.TimeStamp),
			}
			a.OldImportPost(&newPost)
		case sPost.Type == "message" && sPost.SubType == "bot_message":
			if botUser == nil {
				l4g.Warn(utils.T("api.slackimport.slack_add_posts.bot_user_no_exists.warn"))
				continue
			} else if sPost.BotId == "" {
				l4g.Warn(utils.T("api.slackimport.slack_add_posts.no_bot_id.warn"))
				continue
			}

			props := make(model.StringInterface)
			props["override_username"] = sPost.BotUsername
			if len(sPost.Attachments) > 0 {
				props["attachments"] = sPost.Attachments
			}

			post := &model.Post{
				UserId:    botUser.Id,
				ChannelId: channel.Id,
				CreateAt:  SlackConvertTimeStamp(sPost.TimeStamp),
				Message:   sPost.Text,
				Type:      model.POST_SLACK_ATTACHMENT,
			}

			a.OldImportIncomingWebhookPost(post, props)
		case sPost.Type == "message" && (sPost.SubType == "channel_join" || sPost.SubType == "channel_leave"):
			if sPost.User == "" {
				l4g.Debug(utils.T("api.slackimport.slack_add_posts.msg_no_usr.debug"))
				continue
			} else if users[sPost.User] == nil {
				l4g.Debug(utils.T("api.slackimport.slack_add_posts.user_no_exists.debug"), sPost.User)
				continue
			}

			var postType string
			if sPost.SubType == "channel_join" {
				postType = model.POST_JOIN_CHANNEL
			} else {
				postType = model.POST_LEAVE_CHANNEL
			}

			newPost := model.Post{
				UserId:    users[sPost.User].Id,
				ChannelId: channel.Id,
				Message:   sPost.Text,
				CreateAt:  SlackConvertTimeStamp(sPost.TimeStamp),
				Type:      postType,
				Props: model.StringInterface{
					"username": users[sPost.User].Username,
				},
			}
			a.OldImportPost(&newPost)
		case sPost.Type == "message" && sPost.SubType == "me_message":
			if sPost.User == "" {
				l4g.Debug(utils.T("api.slackimport.slack_add_posts.without_user.debug"))
				continue
			} else if users[sPost.User] == nil {
				l4g.Debug(utils.T("api.slackimport.slack_add_posts.user_no_exists.debug"), sPost.User)
				continue
			}
			newPost := model.Post{
				UserId:    users[sPost.User].Id,
				ChannelId: channel.Id,
				Message:   "*" + sPost.Text + "*",
				CreateAt:  SlackConvertTimeStamp(sPost.TimeStamp),
			}
			a.OldImportPost(&newPost)
		case sPost.Type == "message" && sPost.SubType == "channel_topic":
			if sPost.User == "" {
				l4g.Debug(utils.T("api.slackimport.slack_add_posts.msg_no_usr.debug"))
				continue
			} else if users[sPost.User] == nil {
				l4g.Debug(utils.T("api.slackimport.slack_add_posts.user_no_exists.debug"), sPost.User)
				continue
			}
			newPost := model.Post{
				UserId:    users[sPost.User].Id,
				ChannelId: channel.Id,
				Message:   sPost.Text,
				CreateAt:  SlackConvertTimeStamp(sPost.TimeStamp),
				Type:      model.POST_HEADER_CHANGE,
			}
			a.OldImportPost(&newPost)
		case sPost.Type == "message" && sPost.SubType == "channel_purpose":
			if sPost.User == "" {
				l4g.Debug(utils.T("api.slackimport.slack_add_posts.msg_no_usr.debug"))
				continue
			} else if users[sPost.User] == nil {
				l4g.Debug(utils.T("api.slackimport.slack_add_posts.user_no_exists.debug"), sPost.User)
				continue
			}
			newPost := model.Post{
				UserId:    users[sPost.User].Id,
				ChannelId: channel.Id,
				Message:   sPost.Text,
				CreateAt:  SlackConvertTimeStamp(sPost.TimeStamp),
				Type:      model.POST_PURPOSE_CHANGE,
			}
			a.OldImportPost(&newPost)
		case sPost.Type == "message" && sPost.SubType == "channel_name":
			if sPost.User == "" {
				l4g.Debug(utils.T("api.slackimport.slack_add_posts.msg_no_usr.debug"))
				continue
			} else if users[sPost.User] == nil {
				l4g.Debug(utils.T("api.slackimport.slack_add_posts.user_no_exists.debug"), sPost.User)
				continue
			}
			newPost := model.Post{
				UserId:    users[sPost.User].Id,
				ChannelId: channel.Id,
				Message:   sPost.Text,
				CreateAt:  SlackConvertTimeStamp(sPost.TimeStamp),
				Type:      model.POST_DISPLAYNAME_CHANGE,
			}
			a.OldImportPost(&newPost)
		default:
			l4g.Warn(utils.T("api.slackimport.slack_add_posts.unsupported.warn"), sPost.Type, sPost.SubType)
		}
	}
}

func (a *App) SlackUploadFile(sPost SlackPost, uploads map[string]*zip.File, teamId string, channelId string, userId string) (*model.FileInfo, bool) {
	if sPost.File != nil {
		if file, ok := uploads[sPost.File.Id]; ok {
			openFile, err := file.Open()
			if err != nil {
				l4g.Warn(utils.T("api.slackimport.slack_add_posts.upload_file_open_failed.warn", map[string]interface{}{"FileId": sPost.File.Id, "Error": err.Error()}))
				return nil, false
			}
			defer openFile.Close()

			timestamp := utils.TimeFromMillis(SlackConvertTimeStamp(sPost.TimeStamp))
			uploadedFile, err := a.OldImportFile(timestamp, openFile, teamId, channelId, userId, filepath.Base(file.Name))
			if err != nil {
				l4g.Warn(utils.T("api.slackimport.slack_add_posts.upload_file_upload_failed.warn", map[string]interface{}{"FileId": sPost.File.Id, "Error": err.Error()}))
				return nil, false
			}

			return uploadedFile, true
		} else {
			l4g.Warn(utils.T("api.slackimport.slack_add_posts.upload_file_not_found.warn", map[string]interface{}{"FileId": sPost.File.Id}))
			return nil, false
		}
	} else {
		l4g.Warn(utils.T("api.slackimport.slack_add_posts.upload_file_not_in_json.warn"))
		return nil, false
	}
}

func (a *App) deactivateSlackBotUser(user *model.User) {
	_, err := a.UpdateActive(user, false)
	if err != nil {
		l4g.Warn(utils.T("api.slackimport.slack_deactivate_bot_user.failed_to_deactivate", err))
	}
}

func (a *App) addSlackUsersToChannel(members []string, users map[string]*model.User, channel *model.Channel, log *bytes.Buffer) {
	for _, member := range members {
		if user, ok := users[member]; !ok {
			log.WriteString(utils.T("api.slackimport.slack_add_channels.failed_to_add_user", map[string]interface{}{"Username": "?"}))
		} else {
			if _, err := a.AddUserToChannel(user, channel); err != nil {
				log.WriteString(utils.T("api.slackimport.slack_add_channels.failed_to_add_user", map[string]interface{}{"Username": user.Username}))
			}
		}
	}
}

func SlackSanitiseChannelProperties(channel model.Channel) model.Channel {
	if utf8.RuneCountInString(channel.DisplayName) > model.CHANNEL_DISPLAY_NAME_MAX_RUNES {
		l4g.Warn("api.slackimport.slack_sanitise_channel_properties.display_name_too_long.warn", map[string]interface{}{"ChannelName": channel.DisplayName})
		channel.DisplayName = truncateRunes(channel.DisplayName, model.CHANNEL_DISPLAY_NAME_MAX_RUNES)
	}

	if len(channel.Name) > model.CHANNEL_NAME_MAX_LENGTH {
		l4g.Warn("api.slackimport.slack_sanitise_channel_properties.name_too_long.warn", map[string]interface{}{"ChannelName": channel.DisplayName})
		channel.Name = channel.Name[0:model.CHANNEL_NAME_MAX_LENGTH]
	}

	if utf8.RuneCountInString(channel.Purpose) > model.CHANNEL_PURPOSE_MAX_RUNES {
		l4g.Warn("api.slackimport.slack_sanitise_channel_properties.purpose_too_long.warn", map[string]interface{}{"ChannelName": channel.DisplayName})
		channel.Purpose = truncateRunes(channel.Purpose, model.CHANNEL_PURPOSE_MAX_RUNES)
	}

	if utf8.RuneCountInString(channel.Header) > model.CHANNEL_HEADER_MAX_RUNES {
		l4g.Warn("api.slackimport.slack_sanitise_channel_properties.header_too_long.warn", map[string]interface{}{"ChannelName": channel.DisplayName})
		channel.Header = truncateRunes(channel.Header, model.CHANNEL_HEADER_MAX_RUNES)
	}

	return channel
}

func (a *App) SlackAddChannels(teamId string, slackchannels []SlackChannel, posts map[string][]SlackPost, users map[string]*model.User, uploads map[string]*zip.File, botUser *model.User, log *bytes.Buffer) map[string]*model.Channel {
	// Write Header
	log.WriteString(utils.T("api.slackimport.slack_add_channels.added"))
	log.WriteString("=================\r\n\r\n")

	addedChannels := make(map[string]*model.Channel)
	for _, sChannel := range slackchannels {
		newChannel := model.Channel{
			TeamId:      teamId,
			Type:        model.CHANNEL_OPEN,
			DisplayName: sChannel.Name,
			Name:        SlackConvertChannelName(sChannel.Name, sChannel.Id),
			Purpose:     sChannel.Purpose["value"],
			Header:      sChannel.Topic["value"],
		}
		newChannel = SlackSanitiseChannelProperties(newChannel)

		var mChannel *model.Channel
		if result := <-a.Srv.Store.Channel().GetByName(teamId, sChannel.Name, true); result.Err == nil {
			// The channel already exists as an active channel. Merge with the existing one.
			mChannel = result.Data.(*model.Channel)
			log.WriteString(utils.T("api.slackimport.slack_add_channels.merge", map[string]interface{}{"DisplayName": newChannel.DisplayName}))
		} else if result := <-a.Srv.Store.Channel().GetDeletedByName(teamId, sChannel.Name); result.Err == nil {
			// The channel already exists but has been deleted. Generate a random string for the handle instead.
			newChannel.Name = model.NewId()
			newChannel = SlackSanitiseChannelProperties(newChannel)
		}

		if mChannel == nil {
			// Haven't found an existing channel to merge with. Try importing it as a new one.
			mChannel = a.OldImportChannel(&newChannel)
			if mChannel == nil {
				l4g.Warn(utils.T("api.slackimport.slack_add_channels.import_failed.warn"), newChannel.DisplayName)
				log.WriteString(utils.T("api.slackimport.slack_add_channels.import_failed", map[string]interface{}{"DisplayName": newChannel.DisplayName}))
				continue
			}
		}

		a.addSlackUsersToChannel(sChannel.Members, users, mChannel, log)
		log.WriteString(newChannel.DisplayName + "\r\n")
		addedChannels[sChannel.Id] = mChannel
		a.SlackAddPosts(teamId, mChannel, posts[sChannel.Name], users, uploads, botUser)
	}

	return addedChannels
}

func SlackConvertUserMentions(users []SlackUser, posts map[string][]SlackPost) map[string][]SlackPost {
	var regexes = make(map[string]*regexp.Regexp, len(users))
	for _, user := range users {
		r, err := regexp.Compile("<@" + user.Id + `(\|` + user.Username + ")?>")
		if err != nil {
			l4g.Warn(utils.T("api.slackimport.slack_convert_user_mentions.compile_regexp_failed.warn"), user.Id, user.Username)
			continue
		}
		regexes["@"+user.Username] = r
	}

	// Special cases.
	regexes["@here"], _ = regexp.Compile(`<!here\|@here>`)
	regexes["@channel"], _ = regexp.Compile("<!channel>")
	regexes["@all"], _ = regexp.Compile("<!everyone>")

	for channelName, channelPosts := range posts {
		for postIdx, post := range channelPosts {
			for mention, r := range regexes {
				post.Text = r.ReplaceAllString(post.Text, mention)
				posts[channelName][postIdx] = post
			}
		}
	}

	return posts
}

func SlackConvertChannelMentions(channels []SlackChannel, posts map[string][]SlackPost) map[string][]SlackPost {
	var regexes = make(map[string]*regexp.Regexp, len(channels))
	for _, channel := range channels {
		r, err := regexp.Compile("<#" + channel.Id + `(\|` + channel.Name + ")?>")
		if err != nil {
			l4g.Warn(utils.T("api.slackimport.slack_convert_channel_mentions.compile_regexp_failed.warn"), channel.Id, channel.Name)
			continue
		}
		regexes["~"+channel.Name] = r
	}

	for channelName, channelPosts := range posts {
		for postIdx, post := range channelPosts {
			for channelReplace, r := range regexes {
				post.Text = r.ReplaceAllString(post.Text, channelReplace)
				posts[channelName][postIdx] = post
			}
		}
	}

	return posts
}

func SlackConvertPostsMarkup(posts map[string][]SlackPost) map[string][]SlackPost {
	regexReplaceAllString := []struct {
		regex *regexp.Regexp
		rpl   string
	}{
		// URL
		{
			regexp.MustCompile(`<([^|<>]+)\|([^|<>]+)>`),
			"[$2]($1)",
		},
		// bold
		{
			regexp.MustCompile(`(^|[\s.;,])\*(\S[^*\n]+)\*`),
			"$1**$2**",
		},
		// strikethrough
		{
			regexp.MustCompile(`(^|[\s.;,])\~(\S[^~\n]+)\~`),
			"$1~~$2~~",
		},
		// single paragraph blockquote
		// Slack converts > character to &gt;
		{
			regexp.MustCompile(`(?sm)^&gt;`),
			">",
		},
	}

	regexReplaceAllStringFunc := []struct {
		regex *regexp.Regexp
		fn    func(string) string
	}{
		// multiple paragraphs blockquotes
		{
			regexp.MustCompile(`(?sm)^>&gt;&gt;(.+)$`),
			func(src string) string {
				// remove >>> prefix, might have leading \n
				prefixRegexp := regexp.MustCompile(`^([\n])?>&gt;&gt;(.*)`)
				src = prefixRegexp.ReplaceAllString(src, "$1$2")
				// append > to start of line
				appendRegexp := regexp.MustCompile(`(?m)^`)
				return appendRegexp.ReplaceAllString(src, ">$0")
			},
		},
	}

	for channelName, channelPosts := range posts {
		for postIdx, post := range channelPosts {
			result := post.Text

			for _, rule := range regexReplaceAllString {
				result = rule.regex.ReplaceAllString(result, rule.rpl)
			}

			for _, rule := range regexReplaceAllStringFunc {
				result = rule.regex.ReplaceAllStringFunc(result, rule.fn)
			}
			posts[channelName][postIdx].Text = result
		}
	}

	return posts
}

func (a *App) SlackImport(fileData multipart.File, fileSize int64, teamID string) (*model.AppError, *bytes.Buffer) {
	// Create log file
	log := bytes.NewBufferString(utils.T("api.slackimport.slack_import.log"))

	zipreader, err := zip.NewReader(fileData, fileSize)
	if err != nil || zipreader.File == nil {
		log.WriteString(utils.T("api.slackimport.slack_import.zip.app_error"))
		return model.NewAppError("SlackImport", "api.slackimport.slack_import.zip.app_error", nil, err.Error(), http.StatusBadRequest), log
	}

	var channels []SlackChannel
	var users []SlackUser
	posts := make(map[string][]SlackPost)
	uploads := make(map[string]*zip.File)
	for _, file := range zipreader.File {
		reader, err := file.Open()
		if err != nil {
			log.WriteString(utils.T("api.slackimport.slack_import.open.app_error", map[string]interface{}{"Filename": file.Name}))
			return model.NewAppError("SlackImport", "api.slackimport.slack_import.open.app_error", map[string]interface{}{"Filename": file.Name}, err.Error(), http.StatusInternalServerError), log
		}
		if file.Name == "channels.json" {
			channels, _ = SlackParseChannels(reader)
		} else if file.Name == "users.json" {
			users, _ = SlackParseUsers(reader)
		} else {
			spl := strings.Split(file.Name, "/")
			if len(spl) == 2 && strings.HasSuffix(spl[1], ".json") {
				newposts, _ := SlackParsePosts(reader)
				channel := spl[0]
				if _, ok := posts[channel]; !ok {
					posts[channel] = newposts
				} else {
					posts[channel] = append(posts[channel], newposts...)
				}
			} else if len(spl) == 3 && spl[0] == "__uploads" {
				uploads[spl[1]] = file
			}
		}
	}

	posts = SlackConvertUserMentions(users, posts)
	posts = SlackConvertChannelMentions(channels, posts)
	posts = SlackConvertPostsMarkup(posts)

	addedUsers := a.SlackAddUsers(teamID, users, log)
	botUser := a.SlackAddBotUser(teamID, log)

	a.SlackAddChannels(teamID, channels, posts, addedUsers, uploads, botUser, log)

	if botUser != nil {
		a.deactivateSlackBotUser(botUser)
	}

	a.InvalidateAllCaches()

	log.WriteString(utils.T("api.slackimport.slack_import.notes"))
	log.WriteString("=======\r\n\r\n")

	log.WriteString(utils.T("api.slackimport.slack_import.note1"))
	log.WriteString(utils.T("api.slackimport.slack_import.note2"))
	log.WriteString(utils.T("api.slackimport.slack_import.note3"))

	return nil, log
}

type HipchatRoomInfo struct {
	Name       string `json:"name"`
	Id         int    `json:"id"`
	IsArchived bool   `json:"is_archived"`
	Owner      int    `json:"owner"`
	Privacy    string `json:"privacy"`
}

type HipchatRoom struct {
	RoomInfo HipchatRoomInfo `json:"Room"`
}

type HipchatUserInfo struct {
	Name  string `json:"mention_name"`
	Id    int    `json:"id"`
	Email string `json:"email"`
	Exist bool
}
type HipchatUser struct {
	UserInfo HipchatUserInfo `json:"User"`
}

type HipchatSender struct {
	Id int `json:"id"`
}
type HipchatAttach struct {
	Name string `json:"name"`
	Path string `json:"path"`
	Size int    `json:"size"`
}
type HipchatPostInfo struct {
	Attachement *HipchatAttach `json:"attachment"`
	Message     string         `json:"message"`
	Sender      HipchatSender  `json:"sender"`
	Timestamp   string         `json:"timestamp`
	Type        string         `json:"type`
}
type HipchatPost struct {
	PostInfo HipchatPostInfo `json:"UserMessage"`
}

func getUserFromUserArg(a *App, userArg string) *model.User {
	var user *model.User
	if result := <-a.Srv.Store.User().GetByEmail(userArg); result.Err == nil {
		user = result.Data.(*model.User)
	}

	if user == nil {
		if result := <-a.Srv.Store.User().GetByUsername(userArg); result.Err == nil {
			user = result.Data.(*model.User)
		}
	}

	if user == nil {
		if result := <-a.Srv.Store.User().Get(userArg); result.Err == nil {
			user = result.Data.(*model.User)
		}
	}

	return user
}

func HipchatConvertTimeStamp(ts string) int64 {
	elms := strings.SplitN(ts, " ", 2)
	datetimeString, microSecString := elms[0], elms[1]

	dateTime, err := time.Parse(time.RFC3339, datetimeString)
	if err != nil {
		l4g.Warn(utils.T("api.slackimport.slack_convert_timestamp.bad.warn"))
		return -1
	}

	microSec, err := strconv.ParseInt(microSecString, 10, 64)
	if err != nil {
		l4g.Warn(utils.T("api.slackimport.slack_convert_timestamp.bad.warn"))
		return -1
	}

	// to milisec
	timeStamp := dateTime.Unix()*1000 + microSec + 999/1000

	return timeStamp
}

func (a *App) HipchatUploadFile(timestamp int64, attach *HipchatAttach, teamId string, channelId string, userId string) (*model.FileInfo, bool) {

	openFile, err := os.Open(attach.Path)
	if err != nil {
		fmt.Printf("fail to open: %v\n", err)
		l4g.Warn(utils.T("api.slackimport.slack_add_posts.upload_file_open_failed.warn", map[string]interface{}{"FileId": attach.Path, "Error": err.Error()}))
			return nil, false
	}
	defer openFile.Close()

	timestampTime := utils.TimeFromMillis(timestamp)
	uploadedFile, err := a.OldImportFile(timestampTime, openFile, teamId, channelId, userId, attach.Name)
	if err != nil {
		fmt.Printf("fail to import: %v: %s\n", err, attach.Name)
		l4g.Warn(utils.T("api.slackimport.slack_add_posts.upload_file_upload_failed.warn", map[string]interface{}{"FileId": attach.Path, "Error": err.Error()}))
		return nil, false
	}

	return uploadedFile, true
}

func (a *App) HipchatImport(fileName string, teamId string, roomName string) (*model.AppError, *bytes.Buffer) {
	// Create log file
	log := bytes.NewBufferString(utils.T("api.hipchatimport.hipchat_import.log"))

	baseDir := filepath.Dir(fileName)

	// Decode users
	userFileName := filepath.Join(baseDir, "users.json")
	bytes, err := ioutil.ReadFile(userFileName)
	if err != nil {
		log.WriteString("fail to read users.json")
		return model.NewAppError("HipchatImport", "api.slackimport.slack_import.zip.app_error", nil, err.Error(), http.StatusBadRequest), log
	}

	// Decode users
	userMap := make(map[int]string)
	var hcUsers []HipchatUser
	if err := json.Unmarshal(bytes, &hcUsers); err != nil {
		log.WriteString("fail to load room.json")
		return model.NewAppError("HipchatImport", "api.slackimport.slack_import.zip.app_error", nil, err.Error(), http.StatusBadRequest), log
	}
	for _, u := range hcUsers {
		user := getUserFromUserArg(a, u.UserInfo.Email)
		//uid := ""
		//active := false
		if user == nil {
			u.UserInfo.Exist = false
		} else {
			u.UserInfo.Exist = true
			//uid = user.Id
			//if user.DeleteAt == 0 {
			//	active = true
			//}
			userMap[u.UserInfo.Id] = user.Id
		}
		//fmt.Printf("%20s: %3d: %t: %s: %t\n",
		//	u.UserInfo.Name, u.UserInfo.Id, u.UserInfo.Exist, uid, active);
	}

	bytes, err = ioutil.ReadFile(fileName)
	if err != nil {
		log.WriteString("fail to read room.json")
		return model.NewAppError("HipchatImport", "api.slackimport.slack_import.zip.app_error", nil, err.Error(), http.StatusBadRequest), log
	}

	// Decode rooms
	var hcRooms []HipchatRoom
	if err := json.Unmarshal(bytes, &hcRooms); err != nil {
		log.WriteString("fail to load room.json")
		return model.NewAppError("HipchatImport", "api.slackimport.slack_import.zip.app_error", nil, err.Error(), http.StatusBadRequest), log
	}

	channelId := ""
	roomId := -1
	for _, r := range hcRooms {
		if roomName == r.RoomInfo.Name {
			fmt.Printf("room: %s\n", roomName)
			channel, err := a.GetChannelByName(roomName, teamId)
			if err != nil {
				log.WriteString("No room found")
				return model.NewAppError("HipchatImport", "api.slackimport.slack_import.zip.app_error", nil, err.Error(), http.StatusBadRequest), log
			}
			channelId = channel.Id
			roomId = r.RoomInfo.Id
			//fmt.Printf("%v %+v\n", err, channel)
			break

		}
		//fmt.Printf("%20s: %03d: %t\n", r.RoomInfo.Name, r.RoomInfo.Id, r.RoomInfo.IsArchived)
	}

	fmt.Printf("%s: %d: %s\n", roomName, roomId, channelId)

	roomDir := filepath.Join(baseDir, "rooms", strconv.Itoa(roomId))

	// load history
	historyFileName := filepath.Join(roomDir, "history.json")
	bytes, err = ioutil.ReadFile(historyFileName)
	if err != nil {
		log.WriteString("fail to read history.json")
		return model.NewAppError("HipchatImport", "api.slackimport.slack_import.zip.app_error", nil, err.Error(), http.StatusBadRequest), log
	}

	var hcPosts []HipchatPost
	if err := json.Unmarshal(bytes, &hcPosts); err != nil {
		log.WriteString("fail to load history.json")
		return model.NewAppError("HipchatImport", "api.slackimport.slack_import.zip.app_error", nil, err.Error(), http.StatusBadRequest), log
	}

	nPostAll := len(hcPosts)

	// validate
	for i, p := range hcPosts {
		if p.PostInfo.Type == "" {	// Must be "TopicRoomMessage"
			continue
		}
		if p.PostInfo.Type != "user" {
			log.WriteString("post other than 'user' type")
			fmt.Printf("%v\n", p.PostInfo.Type);
			return model.NewAppError("HipchatImport", "api.slackimport.slack_import.zip.app_error", nil, err.Error(), http.StatusBadRequest), log
		}
		//fmt.Printf("%+v\n", p)

		userId, ok := userMap[p.PostInfo.Sender.Id]
		if !ok {
			log.WriteString("unknown Id")
			return model.NewAppError("HipchatImport", "api.slackimport.slack_import.zip.app_error", nil, err.Error(), http.StatusBadRequest), log
		}

		timeStamp := HipchatConvertTimeStamp(p.PostInfo.Timestamp)
		if timeStamp < 0 {
			log.WriteString("fail to convert timestamp Id")
			return model.NewAppError("HipchatImport", "api.slackimport.slack_import.zip.app_error", nil, err.Error(), http.StatusBadRequest), log
		}

		fmt.Printf("%d/%d: %v: %v: %v\n", i+1, nPostAll, userId, channelId, timeStamp)
		newPost := model.Post{
			UserId:    userId,
			ChannelId: channelId,
			Message:   p.PostInfo.Message,
			CreateAt:  timeStamp,
		}

		if p.PostInfo.Attachement != nil {
			attach := p.PostInfo.Attachement
			attach.Path = filepath.Join(roomDir, "files", attach.Path)
			if fileInfo, ok := a.HipchatUploadFile(timeStamp, attach, teamId, channelId, userId); ok {
				newPost.FileIds = append(newPost.FileIds, fileInfo.Id)
				fmt.Printf("-------%v\n", fileInfo);
			} else {
				fmt.Printf("error\n");
				return nil, log
			}
		}
		a.OldImportPost(&newPost)

		for _, fileId := range newPost.FileIds {
			if result := <-a.Srv.Store.FileInfo().AttachToPost(fileId, newPost.Id); result.Err != nil {
				l4g.Error(utils.T("api.slackimport.slack_add_posts.attach_files.error"), newPost.Id, newPost.FileIds, result.Err)
			}
		}
	}

	a.InvalidateAllCaches()

	return nil, log
}
