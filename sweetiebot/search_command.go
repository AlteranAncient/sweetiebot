package sweetiebot

import (
  "github.com/bwmarrin/discordgo"
  "database/sql"
  "strings"
  "strconv"
)

type SearchCommand struct {
  emotes *EmoteModule
  lock AtomicFlag
  statements map[string][]*sql.Stmt
}

func (c *SearchCommand) Name() string {
  return "Search";  
}
func MsgHighlightMatch(msg string, match string) string {
  if len(match) == 0 { return msg }
  msg = strings.Replace(msg, "**" + match + "**", match, -1) // this trick helps prevent increasing ** being appended repeatedly.
  msg = strings.Replace(msg, "**" + match, match, -1) // helps prevent ** from exploding everywhere because discord is bad at isolation.
  return strings.Replace(msg, match, "**" + match + "**", -1)
}
func (c *SearchCommand) Process(args []string, msg *discordgo.Message) (string, bool) {
  if c.lock.test_and_set() {
    return "```Sorry, I'm busy processing another request right now. Please try again later!```", false
  }
  defer c.lock.clear()
  rangebegin := 0
  rangeend := 5
  users := make([]string, 0, 0)
  userIDs := make([]uint64, 0, 0)
  channels := make([]uint64, 0, 0)
  seconds := -1
  messages := make([]string, 0, 0)

  // Fill in parameters from args
  for _, v := range args {
    switch {
      case v[0] == '*':
        if len(v)<2 {
          rangeend = -1 
        } else {
          s := strings.Split(v[1:], "-")
          if len(s) > 1 {
            rangebegin, _ = strconv.Atoi(s[0])
            rangeend, _ = strconv.Atoi(s[1])
          } else {
            rangeend, _ = strconv.Atoi(s[0])
          }
        }
      case v[0] == '@' || (v[0] == '<' && v[1] == '@'):
        if len(v)<2 {
          return "```Error: No users specified```", false
        }
        users = strings.Split(v, "|")
      case v[0] == '#':
          return "```Error: Unknown channel format " + v + " - Must be an actual recognized channel by discord!```", false
      case (v[0] == '<' && v[1] == '#'):
        if len(v)<2 {
          return "```Error: No channels specified```", false
        }
        s := strings.Split(v, "|")
        for _, c := range s {
          if !channelregex.MatchString(c) {
            return "```Error: Unknown channel format " + c + " - Must be an actual recognized channel by discord!```", false
          }
          channels = append(channels, SBatoi(c[2:len(c)-1]))
        }
      case v[0] == '~':
        if len(v)<2 {
          return "```Error: Invalid number of seconds specified. Expected ~000```", false
        }
        i, err := strconv.Atoi(v[1:])
        if len(v)<2 {
          return "```Error: " + err.Error() + "```", false
        }
        seconds = i
      default:
        messages = append(messages, v)
    }
  }

  // Resolve usernames that aren't IDs to IDs
  for _, v := range users {
    v = strings.TrimSpace(v)
    if userregex.MatchString(v) {
      userIDs = append(userIDs, SBatoi(v[2:len(v)-1]))
    } else {
      IDs := sb.db.FindUsers("%" + v[1:] + "%", 20, 0)
      if len(IDs) == 0 { // we failed to resolve this username, so return an error.
        return "```Error: Could not find any usernames or aliases matching " + v[1:] + "!```", false
      }
      userIDs = append(userIDs, IDs...)
    }
  }

  // If we have no searchable arguments, fail
  if len(messages) + len(userIDs) + len(channels) == 0 {
    return "```Error: no searchable terms specified! You must have either a message, a user, or a channel.```", false
  }

  // Assemble query string and parameter list
  params := make([]interface{}, 0, 3)
  query := ""

  if len(userIDs) > 0 {
    temp := make([]string, 0, len(userIDs))
    for _, v := range userIDs {
      temp = append(temp, "C.Author = ?")
      params = append(params, v)
    }
    query += "(" + strings.Join(temp, " OR ") + ") AND "
  }
  
  if len(channels) > 0 {
    temp := make([]string, 0, len(channels))
    for _, v := range channels {
      temp = append(temp, "C.Channel = ?")
      params = append(params, v)
    }
    query += "(" + strings.Join(temp, " OR ") + ") AND "
  }
  
  message := strings.Join(messages, " ")
  if len(messages) > 0 {
    query += "C.Message LIKE ? AND "
    params = append(params, "%" + message + "%")
  }
  
  if seconds >= 0 {
    query += "C.Timestamp > DATE_SUB(NOW(6), INTERVAL ? SECOND) AND "
    params = append(params, seconds)
  }
      
  if msg.ChannelID != sb.SpoilerChannelID {
    query += "C.Channel != ? AND "
    params = append(params, SBatoi(sb.SpoilerChannelID))
  }
  
  query += "C.ID != ? AND C.Author != ? AND C.Channel != ? AND C.Message NOT LIKE '!search %' ORDER BY C.Timestamp DESC" // Always exclude the message corresponding to the command and all sweetie bot messages (which also prevents trailing ANDs)
  params = append(params, SBatoi(msg.ID))
  params = append(params, SBatoi(sb.SelfID))
  params = append(params, SBatoi(sb.ModChannelID))

  querylimit := query
  if rangeend >= 0 {
    querylimit += " LIMIT ?"
    if rangebegin > 0 {
      querylimit += " OFFSET ?"
    }
  }
  
  // if not cached, prepare the statement and store it in a map.
  stmt, ok := c.statements[querylimit]
  if !ok {
    stmt1, err := sb.db.Prepare("SELECT COUNT(*) FROM chatlog C WHERE " + query)    
    stmt2, err2 := sb.db.Prepare("SELECT U.Username, C.Message, C.Timestamp FROM chatlog C INNER JOIN users U ON C.Author = U.ID WHERE " + querylimit)
    if err == nil { err = err2; }
    if err != nil {
      sb.log.Log(err.Error())
      return "```Error: Failed to prepare statement!```", false
    }
    stmt = []*sql.Stmt{stmt1, stmt2}
    c.statements[querylimit] = stmt
  }
  
  // Execute the statement as a count if appropriate, otherwise retrieve a list of messages and construct a return message from them.
  count := 0
  err := stmt[0].QueryRow(params...).Scan(&count)
  if err == sql.ErrNoRows { 
    return "```Error: Expected 1 row, but got no rows!```", false
  }
  
  if count == 0 { return "```No results found.```", false }
  
  strmatch := " matches"
  if count == 1 { strmatch = " match" } // I hate plural forms
  ret := "```Search results: " + strconv.Itoa(count) + strmatch + ".```\n"
  
  if rangebegin < 0 || rangeend < 0 {
    return ret, false
  }
  
  if rangeend >= 0 {
    if rangebegin > 0 { // rangebegin starts at 1, not 0
      if rangeend - rangebegin > sb.config.Maxsearchresults{
        rangeend = rangebegin + sb.config.Maxsearchresults
      }
      if rangeend - rangebegin < 0 {
        rangeend = rangebegin
      }
      params = append(params, rangeend - rangebegin + 1)
      params = append(params, rangebegin - 1) // adjust this so the beginning starts at 1 instead of 0
    } else {
      if rangeend > sb.config.Maxsearchresults {
        rangeend = sb.config.Maxsearchresults
      }
      params = append(params, rangeend)
    }
  }
  
  q, err := stmt[1].Query(params...)
  sb.log.LogError("Search error: ", err)
  defer q.Close()
  r := make([]PingContext, 0, 5)
  for q.Next() {
     p := PingContext{}
     if err := q.Scan(&p.Author, &p.Message, &p.Timestamp); err == nil {
       r = append(r, p)
     }
  }
  
  if len(r) == 0 {
    return "```No results in range.```", false
  }
  
  for _, v := range r {
    ret += "[" + v.Timestamp.Format("1/2 3:04:05PM") + "] " + v.Author + ": " + MsgHighlightMatch(v.Message, message) + "\n"
  }
  
  ret = strings.Replace(ret, "http://", "http\u200B://", -1)
  ret = strings.Replace(ret, "https://", "https\u200B://", -1)
  return ret, len(r) > 5
  //return c.emotes.emoteban.ReplaceAllStringFunc(ret, emotereplace), len(r) > 5
}
func (c *SearchCommand) Usage() string { 
  return FormatUsage(c, "[*[result-range]] [@user[|@user2|...]] [#channel[|#channel2|...]] [~seconds] [message]", "This is an arbitrary search command run on sweetiebot's 7 day chat log. All parameters are optional and can be input in any order, and will all be combined into a single search as appropriate, but if no searchable parameters are given, the operation will fail. The * parameter specifies what results should be returned. Specifing '*10' will return the first 10 results, while '*5-10' will return the 5th to the 10th result (inclusive). If you ONLY specify a single * character, it will only return a count of the total number of results. @user specifies a target user name to search for. An actual ping will be more effective, as it can directly use the user ID, but a raw username will be searched for in the alias table. Multiple users can be searched for by seperating them with | for \"OR\", but each user must still be prefixed with @ even if it's not a ping. #channel must be an actual channel recognized by discord, which will filter results to that channel. Multiple channels can be specified the same way as users can. Remember that if a username has spaces in it, you have to put the entire username parameter in quotes, not just the username itself! The ~ parameter tells the search to limit it's query back N seconds, so ~600 would query only the last 10 minutes of the chat log. [message] will be constructed from all the remaining unrecognized parameters, so you don't need quotes around the message you're looking for.\n\n Example: !search #manechat @cloud|@JamesNotABot *4 ~600\n This will return the most recent 4 messages said by any user with \"cloud\" in the name, or the user JamesNotABot, in the #manechat channel, in the past 10 minutes.") 
}
func (c *SearchCommand) UsageShort() string { return "Performs a complex search on the chat history." }
func (c *SearchCommand) Roles() []string { return []string{} }
func (c *SearchCommand) Channels() []string { return []string{} }