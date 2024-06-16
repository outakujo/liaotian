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
	"github.com/redis/go-redis/v9"
	"log"
	"net"
	"net/http"
	"regexp"
	"sort"
	"time"
)

func main() {
	var serverId string
	flag.StringVar(&serverId, "serverId", "", "serverId")
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
		Addr: "localhost:6379",
	})
	sm := NewSessionManager(serverId, rcli)
	go sm.SubUserMessage()
	go sm.SubServerMessage()

	app := fiber.New()
	app.Use(Recover())
	app.Use(ErrorHandler())
	app.Use(cors.New())
	app.Get("/", func(c *fiber.Ctx) error {
		return c.SendFile("index.html")
	})
	app.Get("tx.jpg", func(c *fiber.Ctx) error {
		return c.SendFile("tx.jpg")
	})
	app.Post("login", Login(loginSvc(rcli)))

	app.Get("liaotian", CheckWebSocket(), Auth(), websocket.New(func(c *websocket.Conn) {
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
		defer sm.DelUser(userId)
		for {
			if mt, msg, err = c.ReadMessage(); err != nil {
				//if strings.Contains(err.Error(), "close 1001") {
				//	break
				//}
				log.Printf("userId:%v read:%v\n", userId, err)
				break
			}
			err = HandleMessage(sm, mt, msg, userId)
			if err != nil {
				log.Printf("userId:%v handle:%v\n", userId, err)
			}
		}
	}, websocket.Config{Subprotocols: []string{SecWebSocketProtocol}}))

	app.Use(Auth())
	lxr := app.Group("lxr")
	lxr.Get("list", func(c *fiber.Ctx) error {
		us := GetUserMap(c)
		name := us[UserIdKey].(string)
		ls, err := rcli.LRange(context.Background(), name+"_lxr", 0, -1).Result()
		if err != nil {
			return err
		}
		sort.Strings(ls)
		return RespData(c, ls)
	})
	lxr.Delete("", func(c *fiber.Ctx) error {
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
	})
	lxr.Post("", func(c *fiber.Ctx) error {
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
	})

	log.Fatal(app.Listen(":3000"))
}

func loginSvc(rcli *redis.Client) LoginLogic {
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
