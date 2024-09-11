### liaotian

简易匿名聊天网站

#### feature

* 前端原生js无依赖
* **依赖redis**
* 分布式websocket部署
* 消息重发
* 支持语音消息
* 简单持久化，未引入数据库，可自行更改
* 连接安全认证

#### limitation

* 前端使用getUserMedia获取麦克风，以下场景才可以使用：
    * 使用HTTPS
    * file:///URL加载的页面
    * localhost加载的页面
