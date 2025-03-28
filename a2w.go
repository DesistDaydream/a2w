package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"text/template"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/rea1shane/gooooo/data"
	myHttp "github.com/rea1shane/gooooo/http"
	"github.com/rea1shane/gooooo/log"
	myTime "github.com/rea1shane/gooooo/time"
	"github.com/sirupsen/logrus"
)

// Notification Alertmanager 发送的告警通知
type Notification struct {
	Receiver string  `json:"receiver"`
	Status   string  `json:"status"`
	Alerts   []Alert `json:"alerts"`

	GroupLabels       map[string]string `json:"groupLabels"`
	CommonLabels      map[string]string `json:"commonLabels"`
	CommonAnnotations map[string]string `json:"commonAnnotations"`

	ExternalURL string `json:"externalURL"`
}

// Alert 告警实例
type Alert struct {
	Status       string            `json:"status"`
	Labels       map[string]string `json:"labels"`
	Annotations  map[string]string `json:"annotations"`
	StartsAt     time.Time         `json:"startsAt"`
	EndsAt       time.Time         `json:"endsAt"`
	GeneratorURL string            `json:"generatorURL"`
	Fingerprint  string            `json:"fingerprint"`
}

const (
	webhookUrl     = "https://qyapi.weixin.qq.com/cgi-bin/webhook/send?key="
	okMsg          = `{"errcode":0,"errmsg":"ok"}`
	markdownMaxLen = 4096     // markdownMaxLen 企业微信 Markdown 消息体最大长度为 4096
	emptyLine      = "\n\n\n" // emptyLine 在企业微信中，连续至少三个的换行符才被视为一个空行
)

var (
	tmplDir, tmplName string
	// key: 模板文件名称; value: 模板文件路径
	tmplFiles map[string]string = make(map[string]string)
	logger    *logrus.Logger
)

func main() {
	// 解析命令行参数
	logLevel := flag.String("log-level", "info", "日志级别。可选值：debug, info, warn, error")
	addr := flag.String("addr", ":5001", "监听地址。格式: [host]:port")
	flag.StringVar(&tmplDir, "template", "./templates", "模板文件所在目录。")
	flag.Parse()

	// 解析日志级别
	level, err := logrus.ParseLevel(*logLevel)
	if err != nil {
		logrus.Panicf("日志级别解析失败: %s", *logLevel)
	}

	// 解析模板文件名称，获取所有后缀为 .tmpl 的文件
	files, err := filepath.Glob(filepath.Join(tmplDir, "*.tmpl"))
	if err != nil || len(files) == 0 {
		logrus.Fatalf("无法从 %s 目录获取到模板文件: %v", tmplDir, err)
	}
	for _, file := range files {
		split := strings.Split(file, "/")
		tmplName := split[len(split)-1]
		tmplFiles[tmplName] = file
	}

	// 创建 logger
	logger = logrus.New()
	logger.SetLevel(level)
	formatter := log.NewFormatter()
	formatter.FieldsOrder = []string{"StatusCode", "Latency"}
	logger.SetFormatter(formatter)

	// 创建 Gin
	app := myHttp.NewHandler(logger, 0)

	app.GET("/", health)
	app.POST("/send", send)

	// 启动
	app.Run(*addr)
}

// health 健康检查
func health(c *gin.Context) {
	c.Writer.WriteString("ok")
}

// send 发送消息
func send(c *gin.Context) {
	// 获取 bot key
	key := c.Query("key")
	// 获取模板名称
	tmplNamePrefix := c.Query("tmpl")
	if tmplNamePrefix == "" {
		tmplName = "base.tmpl"
	} else {
		tmplName = fmt.Sprintf("%v.tmpl", tmplNamePrefix)
	}
	logrus.Debugf("将要使用的模板: %v", tmplFiles[tmplName])
	// 获取提醒列表
	mentions, exist := c.GetQueryArray("mention")
	var mentionsBuilder strings.Builder
	if exist {
		mentionsBuilder.WriteString(emptyLine)
		for _, mention := range mentions {
			mentionsBuilder.WriteString(fmt.Sprintf("<@%v>", mention))
		}
	}
	mentionSnippet := mentionsBuilder.String()
	mentionSnippetLen := len(mentionSnippet)

	// 读取 Alertmanager 消息
	body, err := io.ReadAll(c.Request.Body)
	if err != nil {
		e := c.Error(err)
		e.Meta = "读取 Alertmanager 消息失败"
		c.Writer.WriteHeader(http.StatusBadRequest)
		return
	}
	logger.Debugf("Alertmanager request body: %s", string(body))

	// 解析 Alertmanager 消息
	var notification *Notification
	err = data.UnmarshalBytes(body, &notification, data.JsonFormat)
	if err != nil {
		e := c.Error(err)
		e.Meta = "解析 Alertmanager 消息失败"
		c.Writer.WriteHeader(http.StatusBadRequest)
		return
	}

	// 填充模板
	var tfm = make(template.FuncMap)
	tfm["timeFormat"] = timeFormat
	tfm["timeDuration"] = timeDuration
	tfm["timeFromNow"] = timeFromNow
	tmpl := template.Must(template.New(tmplName).Funcs(tfm).ParseFiles(tmplFiles[tmplName]))
	var content bytes.Buffer
	if err := tmpl.Execute(&content, notification); err != nil {
		e := c.Error(err)
		e.Meta = "填充模板失败"
		c.Writer.WriteHeader(http.StatusInternalServerError)
		return
	}

	// 消息分段
	// 为了解决企业微信 Markdown 消息体长度限制问题
	var msgs []string
	if content.Len()+mentionSnippetLen <= markdownMaxLen {
		msgs = append(msgs, content.String()+mentionSnippet)
	} else {
		// 分段消息标识头
		snippetHeader := `<font color="comment">**(%d/%d)**</font>`

		// 单条分段最大长度
		snippetMaxLen := markdownMaxLen - len(snippetHeader) - mentionSnippetLen

		// 消息切割
		fragments := strings.Split(content.String(), emptyLine)

		var snippetBuilder strings.Builder
		snippetBuilder.Grow(snippetMaxLen)

		// 拼接消息
		for _, fragment := range fragments {
			// 切割后的单条消息都过长
			if len(fragment)+len(emptyLine) > snippetMaxLen {
				e := c.Error(fmt.Errorf("切割后的消息长度 %d 仍超出片段长度限制 %d", len(fragment), snippetMaxLen-len(emptyLine)))
				e.Meta = "分段消息失败"
				c.Writer.WriteHeader(http.StatusBadRequest)
				return
			}

			// 拼接消息后超出限制长度
			if snippetBuilder.Len()+len(fragment)+len(emptyLine) > snippetMaxLen {
				// 添加提醒列表
				snippetBuilder.WriteString(mentionSnippet)
				msgs = append(msgs, snippetBuilder.String())
				snippetBuilder.Reset()
				snippetBuilder.Grow(snippetMaxLen)
			}

			snippetBuilder.WriteString(emptyLine)
			snippetBuilder.WriteString(fragment)
		}

		// 添加提醒列表
		snippetBuilder.WriteString(mentionSnippet)
		msgs = append(msgs, snippetBuilder.String())

		// 添加分段头
		for index, snippet := range msgs {
			snippetBuilder.Reset()
			snippetBuilder.Grow(markdownMaxLen)
			snippetBuilder.WriteString(fmt.Sprintf(snippetHeader, index+1, len(msgs)))
			snippetBuilder.WriteString(snippet)
			msgs[index] = snippetBuilder.String()
		}
	}

	for _, msg := range msgs {
		// 请求企业微信
		postBody, _ := json.Marshal(map[string]interface{}{
			"msgtype": "markdown",
			"markdown": map[string]interface{}{
				"content": msg,
			},
		})
		postBodyBuffer := bytes.NewBuffer(postBody)
		wecomResp, err := http.Post(webhookUrl+key, "application/json", postBodyBuffer)
		if err != nil {
			e := c.Error(err)
			e.Meta = "发起企业微信请求失败"
			c.Writer.WriteHeader(http.StatusInternalServerError)
			return
		}

		// 处理请求结果
		wecomRespBody, _ := io.ReadAll(wecomResp.Body)
		wecomResp.Body.Close()
		if wecomResp.StatusCode != http.StatusOK || string(wecomRespBody) != okMsg {
			e := c.Error(fmt.Errorf("%s", string(wecomRespBody)))
			e.Meta = "请求企业微信失败，HTTP Code: " + strconv.Itoa(wecomResp.StatusCode)
			c.Writer.WriteHeader(http.StatusInternalServerError)
			return
		}
	}

	c.Writer.WriteHeader(http.StatusOK)
}

// timeFormat 格式化时间
func timeFormat(t time.Time) string {
	return t.In(time.Local).Format("2006-01-02 15:04:05")
}

// timeDuration 计算结束时间距开始时间的时间差
func timeDuration(startTime, endTime time.Time) string {
	duration := endTime.Sub(startTime)
	return myTime.FormatDuration(duration)
}

// timeFromNow 计算当前时间距开始时间的时间差
func timeFromNow(startTime time.Time) string {
	return timeDuration(startTime, time.Now())
}
