package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"github.com/go-emix/utils/uuid"
	"github.com/gofiber/contrib/websocket"
	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/fiber/v2/middleware/cors"
	"github.com/golang-jwt/jwt/v5"
	guuid "github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"log"
	"net"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

const audioStatic = "audio"

func main() {
	var serverId string
	var audioDir string
	var redisAddr string
	var serPort int
	flag.StringVar(&serverId, "serverId", "", "serverId")
	flag.StringVar(&audioDir, "audioDir", "audio", "audio save dir")
	flag.StringVar(&redisAddr, "redisAddr", "localhost:6379", "redis server addr")
	flag.IntVar(&serPort, "serverPort", 3000, "serverId")
	flag.Parse()
	if serverId == "" {
		addr, err := getMacAddr()
		if err != nil {
			panic(err)
		}
		serverId = uuid.NewString([]byte(addr))
	}
	log.Printf("serverId:%v\n", serverId)
	rcli := redis.NewClient(&redis.Options{
		Addr: redisAddr,
	})
	os.MkdirAll(audioDir, os.ModePerm)

	app := fiber.New()
	app.Use(Recover())
	app.Use(ErrorHandler())
	app.Use(cors.New())
	app.Get("/", sendFile("index.html"))
	app.Get("tx.jpg", sendFile("tx.jpg"))
	app.Static(audioStatic, audioDir)
	app.Post("login", Login(loginSvc(rcli, serverId)))
	app.Get("liaotian", CheckWebSocket(), Auth(),
		liaotian(rcli, serverId))

	app.Use(Auth())
	app.Post("putaudio", putAudio(audioDir, audioStatic))
	lxr := app.Group("lxr")
	lxr.Get("list", lxrList(rcli))
	lxr.Delete("", deleteLxr(rcli))
	lxr.Post("", addLxr(rcli))

	log.Fatal(app.Listen(":" + strconv.Itoa(serPort)))
}

func sendFile(fn string) fiber.Handler {
	return func(c *fiber.Ctx) error {
		return c.SendFile(fn)
	}
}

func liaotian(rcli *redis.Client, serverId string) fiber.Handler {
	sm := NewSessionManager(serverId, rcli)
	go sm.SubUserMessage()
	go sm.SubServerMessage()
	return websocket.New(func(c *websocket.Conn) {
		var (
			mt  int
			msg []byte
			err error
		)
		us := GetUserMapByWebsocket(c)
		us[UserConnKey] = c
		userId := us[UserIdKey].(string)
		err = sm.AddUser(us)
		if err != nil {
			log.Printf("%v adduser:%v\n", userId, err)
			return
		}
		sms, err := sm.GetUnSendMsg(userId)
		if err != nil {
			log.Printf("%v GetUnSendMsg:%v\n", userId, err)
			return
		}
		for _, m := range sms {
			err = sm.Send(m)
			if err != nil {
				log.Printf("%v send UnSendMsg:%v\n", userId, err)
				continue
			}
		}
		defer sm.DelUser(userId)
		for {
			if mt, msg, err = c.ReadMessage(); err != nil {
				if strings.Contains(err.Error(), "close 1001") {
					break
				}
				log.Printf("userId:%v read:%v\n", userId, err)
				break
			}
			err = HandleMessage(sm, mt, msg, userId)
			if err != nil {
				log.Printf("userId:%v handle:%v\n", userId, err)
			}
		}
	}, websocket.Config{Subprotocols: []string{SecWebSocketProtocol}})
}

func putAudio(dir, static string) fiber.Handler {
	return func(c *fiber.Ctx) error {
		file, err := c.FormFile("file")
		if err != nil {
			return err
		}
		seconds := c.FormValue("time")
		uids := strings.ReplaceAll(guuid.NewString(), "-", "")
		fn := uids + fmt.Sprintf("_%s", seconds) + ".ogg"
		err = c.SaveFile(file, dir+"/"+fn)
		if err != nil {
			return err
		}
		return RespData(c, static+"/"+fn)
	}
}

func addLxr(rcli *redis.Client) fiber.Handler {
	return func(c *fiber.Ctx) error {
		us := GetUserMap(c)
		name := us[UserIdKey].(string)
		id := c.FormValue("id")
		if id == "" {
			return errors.New("id不能为空")
		}
		rcli.LRem(context.Background(), name+"_lxr", 0, id)
		err := rcli.LPush(context.Background(), name+"_lxr", id).Err()
		if err != nil {
			return err
		}
		return RespData(c, nil)
	}
}

func deleteLxr(rcli *redis.Client) fiber.Handler {
	return func(c *fiber.Ctx) error {
		us := GetUserMap(c)
		name := us[UserIdKey].(string)
		id := c.Query("id")
		if id == "" {
			return errors.New("id不能为空")
		}
		err := rcli.LRem(context.Background(), name+"_lxr", 0, id).Err()
		if err != nil {
			return err
		}
		return RespData(c, nil)
	}
}

func lxrList(rcli *redis.Client) fiber.Handler {
	return func(c *fiber.Ctx) error {
		us := GetUserMap(c)
		name := us[UserIdKey].(string)
		ls, err := rcli.LRange(context.Background(), name+"_lxr", 0, -1).Result()
		if err != nil {
			return err
		}
		sort.Strings(ls)
		return RespData(c, ls)
	}
}

func loginSvc(rcli *redis.Client, serverId string) LoginLogic {
	return func(c *fiber.Ctx) (jwt.MapClaims, error) {
		claims := jwt.MapClaims{}
		name := c.FormValue(UserIdKey)
		pass := c.FormValue("pass")
		if name == "" {
			err := SysError{
				Code: http.StatusUnauthorized,
				Msg:  "用户不能为空",
			}
			return claims, err
		}
		if pass == "" {
			err := SysError{
				Code: http.StatusUnauthorized,
				Msg:  "密码不能为空",
			}
			return claims, err
		}
		if ma, _ := regexp.MatchString("^([0-9]|[a-zA-Z]|[_])+$", name); !ma {
			err := SysError{
				Code: http.StatusUnauthorized,
				Msg:  "用户名只能为字母或数字或下划线",
			}
			return claims, err
		}
		key := name + "_pass"
		rs, _ := rcli.Get(context.Background(), key).Result()
		if rs == "" {
			err := rcli.Set(context.Background(), key, pass, -1).Err()
			if err != nil {
				return claims, err
			}
		} else {
			if pass != rs {
				err := SysError{
					Code: http.StatusUnauthorized,
					Msg:  fmt.Sprintf("%v已经存在", name),
				}
				return claims, err
			}
		}
		claims[UserIdKey] = name
		claims[ServerIdKey] = serverId
		claims[UserExpireKey] = time.Now().Add(time.Hour * 1).Unix()
		return claims, nil
	}
}

func getMacAddr() (string, error) {
	netInterfaces, err := net.Interfaces()
	if err != nil {
		return "", err
	}
	for _, netInterface := range netInterfaces {
		macAddr := netInterface.HardwareAddr.String()
		if macAddr != "" {
			return macAddr, nil
		}
	}
	return "", fmt.Errorf("无法获取mac地址")
}
