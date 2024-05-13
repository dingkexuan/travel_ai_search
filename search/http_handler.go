package search

import (
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"
	"travel_ai_search/search/common"
	"travel_ai_search/search/conf"
	"travel_ai_search/search/llm"
	"travel_ai_search/search/llm/dashscope"
	"travel_ai_search/search/llm/spark"
	"travel_ai_search/search/manage"
	"travel_ai_search/search/rewrite"
	searchengineapi "travel_ai_search/search/search_engine_api"
	"travel_ai_search/search/user"

	"github.com/devinyf/dashscopego/qwen"
	"github.com/gin-gonic/gin"
	"github.com/gin-gonic/gin/binding"
	"github.com/gorilla/websocket"
	logger "github.com/sirupsen/logrus"
	"github.com/tmc/langchaingo/llms"
)

type ChatRequest struct {
	Context string `json:"context"`
	Query   string `json:"query" binding:"required"`
}

func InitData(c *gin.Context) {

	num := manage.ParseData(conf.GlobalConfig, manage.CreateIndex)
	c.JSON(http.StatusOK, gin.H{
		"num": num,
	})
}

func PrintChatPrompt(c *gin.Context) {

	req := ChatRequest{}
	if err := c.ShouldBindBodyWith(&req, binding.JSON); err != nil {
		logger.Errorf("parse {%s} err:%s", c.GetString(gin.BodyBytesKey), err)
		c.JSON(http.StatusOK, gin.H{
			"prompt": conf.ErrHint,
		})
		return
	}
	logger.WithField("req", c.GetString(gin.BodyBytesKey)).Info("request chat prompt")

	engine := &ChatEngine{
		SearchEngine: &searchengineapi.LocalSearchEngine{},
		Prompt: &llm.TravelPrompt{
			MaxLength:    1024,
			PromptPrefix: conf.GlobalConfig.PromptTemplate.TravelPrompt,
		},
		Model: &spark.SparkModel{},
	}

	resp, _ := engine.LLMChatPrompt(req.Query)
	c.JSON(http.StatusOK, gin.H{
		"prompt": resp,
	})
}

func dealChatRequest(curUser user.User, msgData map[string]string, msgListener chan string) {
	go func(userInfo user.User, room string, query string) {
		defer func() {
			if err := recover(); err != nil {
				logger.Errorf("panic err is %s \r\n %s", err, common.GetStack())

				contentResp := llm.ChatStream{
					Type: llm.CHAT_TYPE_TOKENS,
					Body: 0,
					Room: room,
				}
				v, _ := json.Marshal(contentResp)
				msgListener <- string(v)

			}
			close(msgListener)
		}()
		tokens := int64(0)
		answer := ""

		var searchEngine searchengineapi.SearchEngine
		var prompt llm.Prompt
		var model llm.GenModel
		var rewritingEngine rewrite.QueryRewritingEngine
		switch room {
		case "travel":
			searchEngine = &searchengineapi.LocalSearchEngine{}
			prompt = &llm.TravelPrompt{
				MaxLength:    1024,
				PromptPrefix: conf.GlobalConfig.PromptTemplate.TravelPrompt,
			}
			model = &spark.SparkModel{Room: room}
			rewritingEngine = &rewrite.LLMQueryRewritingEngine{
				Model: &dashscope.DashScopeModel{
					ModelName: qwen.QwenTurbo,
					Room:      room,
				},
			}
		case "chat":
			fallthrough
		default:
			//searchEngine = &searchengineapi.GoogleSearchEngine{}
			searchEngine = &searchengineapi.OpenSerpSearchEngine{
				Engines: conf.GlobalConfig.OpenSerpSearch.Engines,
				BaseUrl: conf.GlobalConfig.OpenSerpSearch.Url,
			}
			prompt = &llm.ChatPrompt{
				MaxLength:    1024,
				PromptPrefix: conf.GlobalConfig.PromptTemplate.ChatPrompt,
			}
			model = &dashscope.DashScopeModel{
				ModelName: qwen.QwenTurbo,
				Room:      room,
			}

			rewritingEngine = &rewrite.LLMQueryRewritingEngine{
				Model: &dashscope.DashScopeModel{
					ModelName: qwen.QwenTurbo,
					Room:      room,
				},
			}
		}

		engine := &ChatEngine{
			SearchEngine:    searchEngine,
			RewritingEnging: rewritingEngine,
			Prompt:          prompt,
			Model:           model,
			Room:            room,
		}

		if conf.GlobalConfig.SparkLLM.IsMock {
			answer, tokens = engine.LLMChatStreamMock(query, msgListener, llm.GetHistoryStoreInstance().LoadChatHistoryForLLM(userInfo.UserId, room))

		} else {
			answer, tokens = engine.LLMChatStream(query, msgListener, llm.GetHistoryStoreInstance().LoadChatHistoryForLLM(userInfo.UserId, room))
		}
		if tokens > 0 && answer != "" {
			llm.GetHistoryStoreInstance().AddChatHistory(userInfo.UserId, room, query, answer)
		}
		contentResp := llm.ChatStream{
			ChatType: string(llms.ChatMessageTypeAI),
			Room:     room,
			Type:     llm.CHAT_TYPE_TOKENS,
			Body:     tokens,
		}
		v, _ := json.Marshal(contentResp)
		msgListener <- string(v)
	}(curUser, string(msgData["room"]), string(msgData["input"]))
}

func dealChatHistory(curUser user.User, msgData map[string]string, msgListener chan string) {
	//用户历史没有区分频道

	room := msgData["room"]
	msgs := llm.GetHistoryStoreInstance().LoadChatHistoryForHuman(curUser.UserId, room)
	seqno := time.Now().UnixNano()
	for i, msg := range msgs {
		contentResp := llm.ChatStream{
			Room:     room,
			ChatType: string(msg.GetType()),
			Type:     llm.CHAT_TYPE_MSG,
			Body:     msg.GetContent(), //strings.ReplaceAll(content, "\n", "<br />"),
			Seqno:    strconv.FormatInt(seqno+int64(i), 10),
		}
		buf, _ := json.Marshal(contentResp)
		msgListener <- string(buf)
	}

}

var chatUpgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool {
		return true
	},
	WriteBufferSize: 1024,
	ReadBufferSize:  1024,
}

func ChatStream(ctx *gin.Context) {
	w, r := ctx.Writer, ctx.Request

	c, err := chatUpgrader.Upgrade(w, r, nil)
	if err != nil {
		logger.Errorf("chat upgrade:%s", err)
		return
	}

	defer c.Close()

	curUser := user.GetCurUser(ctx)

	msgListener := make(chan string, 10)
	defer close(msgListener)

	go func() {
		for respMsg := range msgListener {
			logger.Infof("send to browser:%s", respMsg)

			err := c.WriteMessage(websocket.TextMessage, []byte(respMsg))
			if err != nil {
				logger.Errorf("write message err:%s", err)
				break
			}
		}
	}()

	for {
		mt, message, err := c.ReadMessage()
		if err != nil {
			logger.Errorf("chat read msg:%s", err)
			break
		}
		//ping pong close已经由框架代理了

		switch mt {
		case websocket.TextMessage:
			{
				logger.Infof("read msg:%s", message)
				msgData := make(map[string]string)
				err := json.Unmarshal([]byte(message), &msgData)
				if err != nil {
					logger.Errorf("json unmarshal %s err:%s", message, err)
					break
				}

				if _, ok := msgData["history"]; ok {
					//阻塞式
					dealChatHistory(curUser, msgData, msgListener)
				} else if _, ok := msgData["input"]; ok {
					dealChatRequest(curUser, msgData, msgListener)
				}

				break
			}
		default:
			{
				logger.Errorf("chat read msg type:%d,msg:%v", mt, message)
				break
			}
		}
	}

}

func Chat(c *gin.Context) {

	req := ChatRequest{}
	if err := c.ShouldBindBodyWith(&req, binding.JSON); err != nil {
		logger.Errorf("parse {%s} err:%s", c.GetString(gin.BodyBytesKey), err)
		c.JSON(http.StatusOK, gin.H{
			"prompt": conf.ErrHint,
		})
		return
	}

	engine := &ChatEngine{
		SearchEngine: &searchengineapi.LocalSearchEngine{},
		Prompt: &llm.TravelPrompt{
			MaxLength:    1024,
			PromptPrefix: conf.GlobalConfig.PromptTemplate.TravelPrompt,
		},
		Model: &spark.SparkModel{},
	}
	//logger.WithField("req", c.GetString(gin.BodyBytesKey)).Info("request chat ")
	resp, tokens := engine.LLMChat(req.Query)
	logger.WithField("req", c.GetString(gin.BodyBytesKey)).WithField("chat", resp).Info("request chat")
	c.JSON(http.StatusOK, gin.H{
		"chat":        resp,
		"totalTokens": tokens,
	})
}

func Home(c *gin.Context) {
	c.HTML(http.StatusOK, "chat.tmpl", gin.H{
		"server": template.JSEscapeString(conf.GlobalConfig.ChatAddr),
	})
}

func Index(c *gin.Context) {
	cookie, err := c.Cookie(conf.GlobalConfig.CookieSession)
	if err != nil {
		cookie = ""
	}
	c.HTML(http.StatusOK, "index.html", gin.H{
		"chat_server":  template.JSEscapeString(conf.GlobalConfig.ChatAddr),
		"cookie_key":   conf.GlobalConfig.CookieSession,
		"cookie_value": cookie,
	})
}

func UploadForm(ctx *gin.Context) {
	curUser := user.GetCurUser(ctx)
	//todo:跳转到登录页面
	if curUser.UserId == user.EmpytUser.UserId {
		ctx.JSON(http.StatusForbidden, gin.H{
			"code":    http.StatusForbidden,
			"message": "没有登录",
		})
		return
	}
	parentUpdloadDir := common.GetUploadPath(conf.GlobalConfig)
	userUploadDir := filepath.Join(parentUpdloadDir, curUser.UserId)
	_, err := os.Stat(userUploadDir)
	if err != nil {
		if os.IsNotExist(err) {
			err = os.MkdirAll(userUploadDir, 0750)
			if err != nil {
				if os.IsExist(err) {
					err = nil
				}
			}

		}
	}
	dir, err := os.Open(userUploadDir)

	if err != nil {
		logger.Errorf("open %s err %s", userUploadDir, err.Error())
		ctx.JSON(http.StatusInternalServerError, gin.H{
			"code":    http.StatusInternalServerError,
			"message": err.Error(),
		})
		return
	}
	defer dir.Close()

	files, err := dir.Readdirnames(0)
	if err != nil {
		logger.Errorf("open %s err %s", userUploadDir, err.Error())
		ctx.JSON(http.StatusInternalServerError, gin.H{
			"code":    http.StatusInternalServerError,
			"message": err.Error(),
		})
		return
	}

	ctx.HTML(http.StatusOK, "upload.html", gin.H{
		"user_name": curUser.UserName,
		"fileNames": files,
	})
}

func Upload(ctx *gin.Context) {
	//todo:限制单用户大小
	//todo:异步处理embedding
	//todo:限制文件大小

	curUser := user.GetCurUser(ctx)
	if curUser.UserId == user.EmpytUser.UserId {
		ctx.JSON(http.StatusForbidden, gin.H{
			"code":    http.StatusForbidden,
			"message": "没有登录",
		})
		return
	}
	form, err := ctx.MultipartForm()
	if err != nil {
		ctx.JSON(http.StatusInternalServerError, gin.H{
			"code":    http.StatusInternalServerError,
			"message": "上传失败",
		})
		return
	}

	parentUpdloadDir := common.GetUploadPath(conf.GlobalConfig)
	userUploadDir := filepath.Join(parentUpdloadDir, curUser.UserId)

	fileInfo, err := os.Stat(userUploadDir)
	if err != nil {
		if os.IsNotExist(err) {
			err = os.MkdirAll(userUploadDir, 0750)
			if err != nil {
				if os.IsExist(err) {
					err = nil
				}
			}

		}
	}
	if !fileInfo.IsDir() {
		err = errors.New("server err:upload path is not dir")
	}
	if err != nil {
		ctx.JSON(http.StatusInternalServerError, gin.H{
			"code":    http.StatusInternalServerError,
			"message": "上传失败:" + err.Error(),
		})
		return
	}

	files := form.File["file"]

	for ind, file := range files {
		fileName := file.Filename
		ext := filepath.Ext(fileName)
		fileName = fileName[:len(fileName)-len(ext)]
		timestamp := time.Now().Format("2006_01_02_15_04_05")

		fileName = fmt.Sprintf("%s_%s%s", fileName, timestamp, ext)

		filePath := filepath.Join(userUploadDir, fileName)

		err = ctx.SaveUploadedFile(file, filePath)
		if err != nil {
			logger.Errorf("save %s err:%s", filePath, err.Error())
			break
		}
		logger.Infof("save [%d] file:%s", ind, filePath)
		// err = os.Chmod(filePath, conf.UPLOAD_FILE_MODE)
		// if err != nil {
		// 	logger.Errorf("chmod %s err:%s", filePath, err.Error())
		// 	break
		// }

		err = manage.CreateDocIndex(filePath)
		if err != nil {
			logger.Errorf("create doc:%s index err:%s", filePath, err.Error())
			break
		}

	}
	if err != nil {
		ctx.JSON(http.StatusInternalServerError, gin.H{
			"code":    http.StatusInternalServerError,
			"message": "上传失败:" + err.Error(),
		})
		return
	}
	ctx.JSON(http.StatusOK, gin.H{
		"code":    0,
		"message": "上传成功",
	})

}
