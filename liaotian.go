package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/go-emix/utils/uuid"
	jwtware "github.com/gofiber/contrib/jwt"
	"github.com/gofiber/contrib/websocket"
	"github.com/gofiber/fiber/v2"
	"github.com/golang-jwt/jwt/v5"
	"github.com/redis/go-redis/v9"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const (
	SignedKey            = "123"
	SecWebSocketProtocol = "Sec-Websocket-Protocol"
	JwtContextKey        = "ur"
	UserIdKey            = "name"
	UserConnKey          = "conn"
	UserExpireKey        = "exp"
	UserTokenKey         = "token"
	UserMessageChannel   = "user_message"
	ServerMessageChannel = "server_message"
	ServerListKey        = "server_list"
)

const (
	SysErrCode    = -1
	PanicCode     = -2
	TokenMissCode = 10000 + iota
	TokenExpiredCode
	TokenInvalidCode
)

const (
	UserDropType = 1
)

type SessionManager struct {
	serverId string
	m        map[string]jwt.MapClaims
	redisCli *redis.Client
	mut      sync.Mutex
}

func NewSessionManager(serverId string, redisCli *redis.Client) *SessionManager {
	s := &SessionManager{redisCli: redisCli, serverId: serverId, m: make(map[string]jwt.MapClaims)}
	err := s.redisCli.SAdd(context.Background(), ServerListKey, serverId).Err()
	if err != nil {
		panic(err)
	}
	return s
}

func (s *SessionManager) Send(msg UserMessage) error {
	if msg.ToId == "" {
		return errors.New("toId 不能为空")
	}
	if _, ok := s.m[msg.ToId]; ok {
		return s.SendLocalMsg(msg)
	}
	return s.PubUserMessage(msg)
}

func (s *SessionManager) PubUserMessage(msg UserMessage) error {
	js, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	return s.redisCli.Publish(context.Background(), UserMessageChannel, js).Err()
}

func (s *SessionManager) SendLocalMsg(msg UserMessage) error {
	user, _ := s.m[msg.ToId]
	conn := user[UserConnKey].(*websocket.Conn)
	if conn == nil {
		return fmt.Errorf("%s断开连接", msg.ToId)
	}
	js, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("send json %s", err)
	}
	return conn.WriteMessage(msg.MsgType, js)
}

func (s *SessionManager) AddUser(us jwt.MapClaims) error {
	userId := us[UserIdKey].(string)
	s.mut.Lock()
	s.m[userId] = us
	s.mut.Unlock()
	sm := ServerMessage{
		MsgType:    UserDropType,
		Content:    userId,
		FromServer: s.serverId,
	}
	js, err := json.Marshal(sm)
	if err != nil {
		return err
	}
	return s.redisCli.Publish(context.Background(), ServerMessageChannel, js).Err()
}

func (s *SessionManager) DelUser(us string) {
	s.mut.Lock()
	defer s.mut.Unlock()
	if um, ok := s.m[us]; ok {
		conn := um[UserConnKey].(*websocket.Conn)
		if conn != nil {
			_ = conn.Close()
		}
		delete(s.m, us)
	}
}

func (s *SessionManager) SubUserMessage() {
	sub := s.redisCli.Subscribe(context.Background(), UserMessageChannel)
	for {
		ctx := context.Background()
		rmsg, err := sub.ReceiveMessage(ctx)
		if err != nil {
			log.Printf("%v SubMessage:%v\n", s.serverId, err)
			break
		}
		var um UserMessage
		err = json.Unmarshal([]byte(rmsg.Payload), &um)
		if err != nil {
			log.Printf("%v SubMessage json:%v\n", s.serverId, err)
			continue
		}
		_, ok := s.m[um.ToId]
		if !ok {
			usk := "unsend_" + uuid.NewString([]byte(rmsg.Payload))
			ri, err := s.redisCli.Incr(context.Background(), usk).Result()
			if err != nil {
				log.Printf("%v SubMessage incr:%v\n", s.serverId, err)
				continue
			}
			ln, err := s.redisCli.SCard(context.Background(), ServerListKey).Result()
			if err != nil {
				log.Printf("%v SubMessage LLen:%v\n", s.serverId, err)
				continue
			}
			if ri == ln {
				go func() {
					err := s.redisCli.Del(context.Background(), usk).Err()
					if err != nil {
						log.Printf("%v SubMessage msg:%s delete:%v \n",
							s.serverId, rmsg.Payload, err)
						return
					}
					err = s.SaveUnSendMsg(usk, um.ToId, rmsg.Payload)
					if err != nil {
						log.Printf("%v SubMessage msg:%s save unsend:%v \n",
							s.serverId, rmsg.Payload, err)
						return
					}
				}()
			}
			continue
		}
		err = s.SendLocalMsg(um)
		if err != nil {
			log.Printf("%v SubMessage send:%v\n", s.serverId, err)
		}
	}
	_ = sub.Close()
	time.Sleep(3 * time.Second)
	log.Printf("%v SubMessage retry conn\n", s.serverId)
	s.SubUserMessage()
}

func (s *SessionManager) SaveUnSendMsg(msgId, toId, msg string) error {
	err := s.redisCli.SetNX(context.Background(), toId+"_"+msgId, msg, -1).Err()
	return err
}

func (s *SessionManager) GetUnSendMsg(userId string) ([]UserMessage, error) {
	ums := make([]UserMessage, 0)
	result, err := s.redisCli.Keys(context.Background(), userId+"_unsend_*").Result()
	if err != nil {
		return ums, err
	}
	var wg sync.WaitGroup
	ch := make(chan UserMessage)
	for _, k := range result {
		wg.Add(1)
		go func(k string) {
			val, err := s.redisCli.GetDel(context.Background(), k).Result()
			if err != nil {
				log.Printf("%v GetUnSendMsg get key:%v\n", s.serverId, err)
				return
			}
			var um UserMessage
			err = json.Unmarshal([]byte(val), &um)
			if err != nil {
				log.Printf("%v GetUnSendMsg json:%v\n", s.serverId, err)
			}
			ch <- um
		}(k)
	}
	wg.Wait()
	for i := 0; i < len(result); i++ {
		m := <-ch
		ums = append(ums, m)
	}
	return ums, nil
}

func (s *SessionManager) SubServerMessage() {
	sub := s.redisCli.Subscribe(context.Background(), ServerMessageChannel)
	for {
		ctx := context.Background()
		rmsg, err := sub.ReceiveMessage(ctx)
		if err != nil {
			log.Printf("%v SubServerMessage:%v\n", s.serverId, err)
			break
		}
		var sm ServerMessage
		err = json.Unmarshal([]byte(rmsg.Payload), &sm)
		if err != nil {
			log.Printf("%v SubServerMessage json:%v\n", s.serverId, err)
			continue
		}
		switch sm.MsgType {
		case UserDropType:
			if s.serverId != sm.FromServer {
				log.Printf("%v SubServerMessage user:%s drop\n",
					s.serverId, sm.Content)
				s.DelUser(sm.Content)
			}
		}
	}
	_ = sub.Close()
	time.Sleep(3 * time.Second)
	log.Printf("%v SubServerMessage retry conn\n", s.serverId)
	s.SubServerMessage()
}

type UserMessage struct {
	FromId      string `json:"fromId"`
	ToId        string `json:"toId"`
	Content     string `json:"content"`
	ContentType int    `json:"contentType"`
	Time        int64  `json:"time"`
	MsgType     int    `json:"msgType"`
}

type ServerMessage struct {
	MsgType    int    `json:"msgType"`
	Content    string `json:"content"`
	FromServer string `json:"fromServer"`
}

func HandleMessage(sm *SessionManager, messageType int, p []byte, fromId string) error {
	var um UserMessage
	err := json.Unmarshal(p, &um)
	if err != nil {
		return err
	}
	um.MsgType = messageType
	um.FromId = fromId
	return sm.Send(um)
}

func CheckWebSocket() fiber.Handler {
	return func(c *fiber.Ctx) error {
		err := SysError{}
		auth := c.GetReqHeaders()[SecWebSocketProtocol]
		if len(auth) == 0 {
			err.Code = http.StatusUnauthorized
			err.Msg = fmt.Sprintf("未支持%s",
				SecWebSocketProtocol)
			return err
		}
		token := auth[0]
		if token == "" {
			err.Code = http.StatusUnauthorized
			err.Msg = UserTokenKey + "为空"
			return err
		}
		sp := strings.Split(token, ",")
		if len(sp) != 2 {
			err.Code = http.StatusUnauthorized
			err.Msg = UserTokenKey + "为空"
			return err
		}
		tk, _ := url.PathUnescape(sp[1])
		c.Request().Header.Set("Authorization", strings.TrimSpace(tk))
		if !websocket.IsWebSocketUpgrade(c) {
			err.Code = fiber.StatusUpgradeRequired
			err.Msg = "不支持websocket"
			return err
		}
		return c.Next()
	}
}

func GetUserMap(c *fiber.Ctx) jwt.MapClaims {
	user := c.Locals(JwtContextKey).(*jwt.Token)
	claims := user.Claims.(jwt.MapClaims)
	return claims
}

func GetUserMapByWebsocket(c *websocket.Conn) jwt.MapClaims {
	user := c.Locals(JwtContextKey).(*jwt.Token)
	claims := user.Claims.(jwt.MapClaims)
	return claims
}

func Auth() fiber.Handler {
	return jwtware.New(jwtware.Config{ContextKey: JwtContextKey, ErrorHandler: func(c *fiber.Ctx, err error) error {
		if err == nil {
			return nil
		}
		se := SysError{HttpCode: http.StatusForbidden, Code: TokenInvalidCode, Msg: err.Error()}
		if err == jwtware.ErrJWTMissingOrMalformed {
			se.Code = TokenMissCode
		}
		if strings.Contains(err.Error(), "token is expired") {
			se.Code = TokenExpiredCode
		}
		return se
	},
		SigningKey: jwtware.SigningKey{Key: []byte(SignedKey)}})
}

type LoginLogic func(c *fiber.Ctx) (jwt.MapClaims, error)

func Login(logic LoginLogic) fiber.Handler {
	return func(c *fiber.Ctx) error {
		cli, err := logic(c)
		if err != nil {
			return SysError{
				Code: http.StatusUnauthorized,
				Msg:  err.Error(),
			}
		}
		token := jwt.NewWithClaims(jwt.SigningMethodHS256, cli)
		t, err := token.SignedString([]byte(SignedKey))
		if err != nil {
			return SysError{
				Code: http.StatusInternalServerError,
				Msg:  err.Error(),
			}
		}
		return RespData(c, fiber.Map{UserTokenKey: t})
	}
}

func Recover() fiber.Handler {
	return func(c *fiber.Ctx) error {
		defer func() {
			if err := recover(); err != nil {
				err = RespErrStr(c, PanicCode, fmt.Sprintf("%s", err), http.StatusInternalServerError)
				if err != nil {
					log.Println(err)
				}
			}
		}()
		return c.Next()
	}
}

func ErrorHandler() fiber.Handler {
	return func(c *fiber.Ctx) error {
		err := c.Next()
		if err == nil {
			return nil
		}
		if se, ok := err.(SysError); ok {
			if se.HttpCode == 0 {
				se.HttpCode = se.Code
			}
			return RespErr(c, se.Code, se, se.HttpCode)
		}
		return RespErr(c, SysErrCode, err, http.StatusInternalServerError)
	}
}

type SysError struct {
	Code     int
	HttpCode int
	Msg      string
}

func (s SysError) Error() string {
	return s.Msg
}

type Resp struct {
	Code int    `json:"code"`
	Msg  string `json:"msg,omitempty"`
	Data any    `json:"data,omitempty"`
}

func RespErrStr(c *fiber.Ctx, code int, err string, httpCode ...int) error {
	if len(httpCode) != 0 {
		c = c.Status(httpCode[0])
	} else {
		c = c.Status(code)
	}
	return c.JSON(Resp{
		Code: code,
		Msg:  err,
	})
}

func RespErr(c *fiber.Ctx, code int, err error, httpCode ...int) error {
	return RespErrStr(c, code, err.Error(), httpCode...)
}

func RespData(c *fiber.Ctx, data any) error {
	return c.Status(http.StatusOK).JSON(Resp{
		Data: data,
	})
}
