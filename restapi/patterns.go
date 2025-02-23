package restapi

import (
	"strconv"
	"bufio"
	"log"
	"net/http"
	"os/exec"
	"strings"
	"github.com/PuerkitoBio/goquery"
	"io"
	"time"

	"github.com/danielmiessler/fabric/plugins/db/fsdb"
	"github.com/gin-gonic/gin"
)

// ExecuteRequest and ExecuteResponse definitions remain the same
type ExecuteRequest struct {
	Input   string `json:"input"`
	Stream  bool   `json:"stream"`
	Youtube bool   `json:"youtube"`
	Model   string `json:"model,omitempty"`
	ContextLength int `json:"context_length,omitempty"`
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
	pattern, err := h.patterns.GetApplyVariables(name, variables, "")
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
			if req.Model != "" {
				args = append(args, "--model="+req.Model)
			}
			if req.ContextLength > 0 {
				args = append(args, "--modelContextLength="+strconv.Itoa(req.ContextLength))			
			}
			cmd = exec.Command("/fabric", args...)
		} else {
			// For non-YouTube URLs, use getWebContent function
			content, err := getWebContent(req.Input)
			if err != nil {
				log.Printf("Error fetching content: %v", err)
				c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
				return
			}

			// Create pipe for fabric command
			pipeReader, pipeWriter := io.Pipe()
			go func() {
				defer pipeWriter.Close()
				io.WriteString(pipeWriter, content)
			}()

			fabricArgs := []string{"--pattern", name}
			if req.Stream {
				fabricArgs = append(fabricArgs, "--stream")
			}
			if req.Model != "" {
				fabricArgs = append(fabricArgs, "--model="+req.Model)
			}
			if req.ContextLength > 0 {
				fabricArgs = append(fabricArgs, "--modelContextLength="+strconv.Itoa(req.ContextLength))			
			}
			cmd = exec.Command("/fabric", fabricArgs...)
			cmd.Stdin = pipeReader
		}
	} else {
		// Direct content input
		fabricArgs := []string{"--pattern", name}
		if req.Stream {
			fabricArgs = append(fabricArgs, "--stream")
		}
		if req.Model != "" {
			fabricArgs = append(fabricArgs, "--model="+req.Model)
		}
		if req.ContextLength > 0 {
			fabricArgs = append(fabricArgs, "--modelContextLength="+strconv.Itoa(req.ContextLength))			
		}
		cmd = exec.Command("/fabric", fabricArgs...)
		cmd.Stdin = strings.NewReader(req.Input)
	}

	log.Printf("Executing command: %v", cmd.Args)

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

		// Wait for streaming to complete
		<-done

	} else {
		// For non-streaming requests
		var output []byte
		var err error

		output, err = cmd.CombinedOutput()
		if err != nil {
			log.Printf("Command failed: %v\nOutput: %s", err, string(output))
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}

		c.JSON(http.StatusOK, ExecuteResponse{
			Content: string(output),
		})
	}
}

func getWebContent(url string) (string, error) {
	client := &http.Client{
		Timeout: 30 * time.Second,
	}

	resp, err := client.Get(url)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	contentType := resp.Header.Get("Content-Type")
	
	// Handle HTML content
	if strings.Contains(contentType, "text/html") {
		doc, err := goquery.NewDocumentFromReader(resp.Body)
		if err != nil {
			return "", err
		}
		
		// Remove unwanted elements
		doc.Find("script,style,nav,header,footer").Remove()
		
		// Extract text content
		return strings.TrimSpace(doc.Find("body").Text()), nil
	}
	
	// Handle plain text content
	content, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	
	return string(content), nil
}
