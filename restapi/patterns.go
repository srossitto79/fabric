package restapi

import (
	"bufio"
	"log"
	"net/http"
	"os/exec"
	"strings"

	"github.com/danielmiessler/fabric/plugins/db/fsdb"
	"github.com/gin-gonic/gin"
)

// ExecuteRequest and ExecuteResponse definitions remain the same
type ExecuteRequest struct {
	Input   string `json:"input"`
	Stream  bool   `json:"stream"`
	Youtube bool   `json:"youtube"`
}

type ExecuteResponse struct {
	Content string `json:"content"`
}

type PatternsHandler struct {
	*StorageHandler[fsdb.Pattern]
	patterns *fsdb.PatternsEntity
}

func NewPatternsHandler(r *gin.Engine, patterns *fsdb.PatternsEntity) (ret *PatternsHandler) {
	ret = &PatternsHandler{
		StorageHandler: NewStorageHandler[fsdb.Pattern](r, "patterns", patterns),
		patterns:       patterns,
	}
	r.POST("/patterns/:name/execute", ret.Execute)
	return
}

func (h *PatternsHandler) Get(c *gin.Context) {
	name := c.Param("name")
	variables := make(map[string]string)
	pattern, err := h.patterns.GetApplyVariables(name, variables)
	if err != nil {
		c.JSON(http.StatusInternalServerError, err.Error())
		return
	}
	c.JSON(http.StatusOK, pattern)
}

func (h *PatternsHandler) Execute(c *gin.Context) {
	name := c.Param("name")

	var req ExecuteRequest
	if err := c.BindJSON(&req); err != nil {
		log.Printf("Error binding JSON: %v", err)
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	var cmd *exec.Cmd
	var wgetCmd *exec.Cmd

	// Prepare command based on input type and flags
	if strings.HasPrefix(req.Input, "http://") || strings.HasPrefix(req.Input, "https://") {
		if req.Youtube || name == "transcript" {
			args := []string{"-y", req.Input, "--transcript"}
			if name != "transcript" {
				args = append(args, "--pattern", name)
				if req.Stream {
					args = append(args, "--stream")
				}
			}
			cmd = exec.Command("/fabric", args...)
		} else {
			// For non-YouTube URLs, use wget pipeline
			wgetCmd = exec.Command("wget", "-qO-", req.Input)
			fabricArgs := []string{"--pattern", name}
			if req.Stream {
				fabricArgs = append(fabricArgs, "--stream")
			}
			cmd = exec.Command("/fabric", fabricArgs...)

			// Set up pipe between commands
			var err error
			cmd.Stdin, err = wgetCmd.StdoutPipe()
			if err != nil {
				log.Printf("Error creating pipe: %v", err)
				c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
				return
			}
		}
	} else {
		// Direct content input
		fabricArgs := []string{"--pattern", name}
		if req.Stream {
			fabricArgs = append(fabricArgs, "--stream")
		}
		cmd = exec.Command("/fabric", fabricArgs...)
		cmd.Stdin = strings.NewReader(req.Input)
	}

	log.Printf("Executing command: %v", cmd.Args)
	if wgetCmd != nil {
		log.Printf("With wget command: %v", wgetCmd.Args)
	}

	if req.Stream {
		// Set required headers for SSE
		c.Header("Content-Type", "text/event-stream")
		c.Header("Cache-Control", "no-cache")
		c.Header("Connection", "keep-alive")
		c.Header("X-Accel-Buffering", "no") // Disable proxy buffering

		// Flush headers immediately
		c.Writer.Flush()

		stdout, err := cmd.StdoutPipe()
		if err != nil {
			log.Printf("Error creating stdout pipe: %v", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}

		stderr, err := cmd.StderrPipe()
		if err != nil {
			log.Printf("Error creating stderr pipe: %v", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}

		// Start wget if it exists
		if wgetCmd != nil {
			if err := wgetCmd.Start(); err != nil {
				log.Printf("Error starting wget: %v", err)
				c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
				return
			}
		}

		if err := cmd.Start(); err != nil {
			log.Printf("Error starting command: %v", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}

		// Handle stderr in a goroutine
		go func() {
			scanner := bufio.NewScanner(stderr)
			for scanner.Scan() {
				log.Printf("stderr: %s", scanner.Text())
			}
		}()

		// Create a channel to signal when streaming is done
		done := make(chan bool)

		// Stream stdout in a goroutine
		go func() {
			scanner := bufio.NewScanner(stdout)
			for scanner.Scan() {
				line := scanner.Text()
				// Send SSE message and flush immediately
				c.SSEvent("message", ExecuteResponse{Content: line})
				c.Writer.Flush()
			}
			if err := scanner.Err(); err != nil {
				log.Printf("Error reading stdout: %v", err)
				c.SSEvent("error", gin.H{"error": err.Error()})
				c.Writer.Flush()
			}
			done <- true
		}()

		// Wait for command completion
		if err := cmd.Wait(); err != nil {
			log.Printf("Command failed: %v", err)
			c.SSEvent("error", gin.H{"error": err.Error()})
			c.Writer.Flush()
		}

		// Wait for wget completion if it exists
		if wgetCmd != nil {
			if err := wgetCmd.Wait(); err != nil {
				log.Printf("Wget command failed: %v", err)
			}
		}

		// Wait for streaming to complete
		<-done

	} else {
		// For non-streaming requests
		var output []byte
		var err error

		// Start wget if it exists
		if wgetCmd != nil {
			if err := wgetCmd.Start(); err != nil {
				log.Printf("Error starting wget: %v", err)
				c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
				return
			}
		}

		output, err = cmd.CombinedOutput()
		if err != nil {
			log.Printf("Command failed: %v\nOutput: %s", err, string(output))
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}

		// Wait for wget completion if it exists
		if wgetCmd != nil {
			if err := wgetCmd.Wait(); err != nil {
				log.Printf("Wget command failed: %v", err)
			}
		}

		c.JSON(http.StatusOK, ExecuteResponse{
			Content: string(output),
		})
	}
}
