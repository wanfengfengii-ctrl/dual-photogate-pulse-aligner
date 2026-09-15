// Command server 启动包装线脉冲配对的纯后端 HTTP API。
package main

import (
	"log"
	"net/http"
	"os"
	"time"

	"github.com/gin-gonic/gin"

	"packageline/internal/api"
)

func main() {
	port := os.Getenv("API_PORT")
	if port == "" {
		port = "8080"
	}
	if _, ok := os.LookupEnv("GIN_MODE"); !ok {
		gin.SetMode(gin.ReleaseMode)
	}

	r := gin.New()
	r.Use(gin.Recovery())
	r.Use(requestLogger())

	api.NewHandler().Register(r)

	srv := &http.Server{
		Addr:              ":" + port,
		Handler:           r,
		ReadHeaderTimeout: 5 * time.Second,
	}
	log.Printf("packageline API listening on :%s", port)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("server error: %v", err)
	}
}

func requestLogger() gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		c.Next()
		log.Printf("%s %s -> %d (%s)", c.Request.Method, c.Request.URL.Path, c.Writer.Status(), time.Since(start))
	}
}
