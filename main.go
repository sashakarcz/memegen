package main

import (
    "context"
    "database/sql"
    "encoding/json"
    "fmt"
    "html/template"
    "io"
    "log"
    "net/http"
    "os"
    "strings"
    "time"

    "github.com/gofiber/fiber/v2"
    "github.com/redis/go-redis/v9"

    _ "github.com/lib/pq"
)

// Meme struct (metadata only)
type Meme struct {
    ID       int
    Template string
    Lines    string // JSON array to store multiple lines
    URL      string
    Context  string
    Link     string
    Votes    int
    Comments []Comment
}

// Comment struct
type Comment struct {
    ID        int
    MemeID    int
    Author    string
    Content   string
    CreatedAt time.Time
}

// Memegen Template struct
type MemeTemplate struct {
    ID       string `json:"id"`
    Name     string `json:"name"`
    Lines    int    `json:"lines"`
    BlankURL string `json:"blank"`
    Example  struct {
        URL string `json:"url"`
    } `json:"example"`
}

var (
    db         *sql.DB
    redisClient *redis.Client
    memegenAPI  = "http://memegen:5000"
    ctx        = context.Background()
)

func getEnv(key, defaultValue string) string {
    if value := os.Getenv(key); value != "" {
        return value
    }
    return defaultValue
}

func main() {
    var err error
    
    // Get database connection parameters from environment
    dbHost := getEnv("DB_HOST", "localhost")
    dbPort := getEnv("DB_PORT", "5432")
    dbUser := getEnv("DB_USER", "memegen")
    dbPassword := getEnv("DB_PASSWORD", "memegen_password")
    dbName := getEnv("DB_NAME", "memegen")
    
    connStr := fmt.Sprintf("host=%s port=%s user=%s password=%s dbname=%s sslmode=disable",
        dbHost, dbPort, dbUser, dbPassword, dbName)
    db, err = sql.Open("postgres", connStr)
    if err != nil {
        log.Fatal(err)
    }
    createTable()

    // Initialize Redis for caching images
    redisHost := getEnv("REDIS_HOST", "localhost")
    redisPort := getEnv("REDIS_PORT", "6379")
    redisClient = redis.NewClient(&redis.Options{
        Addr:     fmt.Sprintf("%s:%s", redisHost, redisPort),
        Password: "",
        DB:       0,
    })

    app := fiber.New()
    app.Static("/static", "./static")

    // Route: Homepage (shows all memes)
    app.Get("/", func(c *fiber.Ctx) error {
        memes := getAllMemes()
        return renderTemplate(c, "index.html", memes)
    })

    // Route: Meme Creation Form
    app.Get("/generate", func(c *fiber.Ctx) error {
        templates, _ := fetchMemegenTemplates()
        return renderTemplate(c, "meme_form.html", templates)
    })
        // Route: Delete Meme (Requires Admin Key)
    app.Delete("/delete/:id", func(c *fiber.Ctx) error {
        memeID := c.Params("id")
        adminKey := c.Query("key") // Get key from request query

        // Compare key with expected admin key
        expectedKey := getEnv("ADMIN_KEY", "CHANGEME")
        if adminKey != expectedKey {
            return c.Status(403).JSON(fiber.Map{"error": "Unauthorized"})
        }

        // Retrieve meme URL before deleting it from the database
        var memeURL string
        err := db.QueryRow("SELECT url FROM memes WHERE id = $1", memeID).Scan(&memeURL)
        if err != nil {
            return c.Status(404).JSON(fiber.Map{"error": "Meme not found"})
        }

        // Delete meme from database
        _, err = db.Exec("DELETE FROM memes WHERE id = $1", memeID)
        if err != nil {
            return c.Status(500).JSON(fiber.Map{"error": "Database error"})
        }

        // Remove meme from Redis
        err = redisClient.Del(ctx, memeURL).Err()
        if err != nil {
            log.Println("Error removing meme from Redis:", err)
        } else {
            log.Printf("Deleted meme from Redis: %s", memeURL)
        }

        return c.JSON(fiber.Map{"success": "Meme deleted"})
    })

    // Route: Handle Meme Voting with Redis User Tracking
    app.Post("/vote/:id/:direction", func(c *fiber.Ctx) error {
        memeID := c.Params("id")
        direction := c.Params("direction")

        // Get user identifier (use IP address for simplicity)
        userIP := c.IP()
        voteKey := fmt.Sprintf("vote:%s:%s", memeID, userIP) // Unique key for user vote

        // Check if user already voted
        previousVote, err := redisClient.Get(ctx, voteKey).Result()

        if err == nil { // User has already voted before
            if previousVote == direction {
                return c.JSON(fiber.Map{"error": "You have already voted this way", "votes": getMemeVotes(memeID)})
            }

            // Reverse previous vote first
            if previousVote == "up" {
                _, _ = db.Exec("UPDATE memes SET votes = votes - 1 WHERE id = $1", memeID)
            } else if previousVote == "down" {
                _, _ = db.Exec("UPDATE memes SET votes = votes + 1 WHERE id = $1", memeID)
            }
        }

        // Apply the new vote
        if direction == "up" {
            _, err = db.Exec("UPDATE memes SET votes = votes + 1 WHERE id = $1", memeID)
        } else if direction == "down" {
            _, err = db.Exec("UPDATE memes SET votes = votes - 1 WHERE id = $1", memeID)
        } else {
            return c.Status(400).JSON(fiber.Map{"error": "Invalid vote direction"})
        }

        if err != nil {
            return c.Status(500).SendString("Database error")
        }

        // Store vote in Redis (expire in 7 days)
        err = redisClient.Set(ctx, voteKey, direction, 604800*time.Second).Err()
        if err != nil {
            log.Println("Error storing user vote in Redis:", err)
        }

        // Return updated vote count
        return c.JSON(fiber.Map{"votes": getMemeVotes(memeID)})
    })

    // Route: Add Comment to Meme
    app.Post("/comment/:id", func(c *fiber.Ctx) error {
        memeID := c.Params("id")
        author := c.FormValue("author")
        content := c.FormValue("content")

        if author == "" || content == "" {
            return c.Status(400).JSON(fiber.Map{"error": "Author and content are required"})
        }

        // Insert comment into database
        _, err := db.Exec("INSERT INTO comments (meme_id, author, content) VALUES ($1, $2, $3)",
            memeID, author, content)
        if err != nil {
            log.Println("Error inserting comment:", err)
            return c.Status(500).JSON(fiber.Map{"error": "Database error"})
        }

        return c.JSON(fiber.Map{"success": "Comment added successfully"})
    })

    // Route: Generate Meme
    app.Post("/generate", func(c *fiber.Ctx) error {
        templateName := c.FormValue("template")

        // Collect multiple lines
        var lines []string
        for i := 1; i <= 10; i++ {
            line := c.FormValue(fmt.Sprintf("line%d", i))
            if line != "" {
                lines = append(lines, line)
            }
        }

        if len(lines) == 0 {
            return c.Status(400).SendString("At least one line of text is required.")
        }

        // Convert lines to JSON for storage
        linesJSON, _ := json.Marshal(lines)

        // Generate URL for meme
        textParams := strings.Join(lines, "/")
        url := fmt.Sprintf("%s/images/%s/%s.png", memegenAPI, templateName, textParams)

        // Save meme
        context := c.FormValue("context")
        link := c.FormValue("link")
        saveMeme(templateName, string(linesJSON), url, context, link)

        return c.Redirect("/")
    })

    // Proxy Memegen API and cache images in Redis
    app.Get("/api/images/:template/*", func(c *fiber.Ctx) error {
        template := c.Params("template")
    
        // Get all remaining parts of the path as the text lines
        textParts := strings.Split(c.Params("*"), "/")
    
        // If no text parts exist, use a default placeholder
        if len(textParts) == 0 {
            textParts = []string{"_"}
        }

        // Construct Redis cache key
        cacheKey := fmt.Sprintf("meme:%s:%s", template, strings.Join(textParts, ":"))

        // Check Redis cache first
        imageBytes, err := redisClient.Get(ctx, cacheKey).Bytes()
        if err == nil {
            log.Printf("Serving image from Redis: %s", cacheKey)
            c.Set("Content-Type", "image/png")
            return c.Send(imageBytes)
        }

        // Construct Memegen API URL dynamically
        memegenURL := fmt.Sprintf("%s/images/%s/%s.png", memegenAPI, template, strings.Join(textParts, "/"))
        log.Println("Fetching meme from API:", memegenURL)

        // Fetch the image from Memegen API
        resp, err := http.Get(memegenURL)
        if err != nil {
            log.Println("Error fetching image:", err)
            return c.Status(500).SendString("Error fetching meme image")
        }
        defer resp.Body.Close()

        if resp.StatusCode == http.StatusNotFound {
            log.Println("Memegen API returned 404:", memegenURL)
            return c.Status(404).SendString("Meme not found")
        }

        // Read image into memory
        imageBytes, err = io.ReadAll(resp.Body)
        if err != nil {
            log.Println("Error reading image:", err)
            return c.Status(500).SendString("Error processing meme image")
        }

        // Cache image in Redis (expires in 7 days)
        err = redisClient.Set(ctx, cacheKey, imageBytes, 604800*time.Second).Err()
        if err != nil {
            log.Println("Error caching image in Redis:", err)
        } else {
            log.Printf("Cached image in Redis: %s", cacheKey)
        }

        // Serve the image
        c.Set("Content-Type", "image/png")
        return c.Send(imageBytes)
    })
 
    // Route: Serve Meme via Redis
    app.Get("/meme/:id", serveMeme)

    // Route: Meme Detail Page
    app.Get("/meme/:id/view", func(c *fiber.Ctx) error {
        memeID := c.Params("id")
        
        // Get meme details from database
        var meme Meme
        var linesJSON string
        err := db.QueryRow("SELECT id, template, lines, url, context, link, votes FROM memes WHERE id = $1", memeID).Scan(
            &meme.ID, &meme.Template, &linesJSON, &meme.URL, &meme.Context, &meme.Link, &meme.Votes)
        if err != nil {
            return c.Status(404).SendString("Meme not found")
        }

        // Decode JSON lines
        var lines []string
        json.Unmarshal([]byte(linesJSON), &lines)
        meme.Lines = strings.Join(lines, "\n")

        // Get comments for this meme
        meme.Comments = getCommentsForMeme(meme.ID)

        return renderTemplate(c, "meme_detail.html", meme)
    })

    log.Fatal(app.Listen(":8181"))
}

// Create DB table
func createTable() {
    memeQuery := `CREATE TABLE IF NOT EXISTS memes (
        id SERIAL PRIMARY KEY,
        template TEXT,
        lines TEXT,  -- JSON encoded array of lines
        url TEXT,
        context TEXT DEFAULT '',
        link TEXT DEFAULT '',
        votes INTEGER DEFAULT 0
    )`
    _, err := db.Exec(memeQuery)
    if err != nil {
        log.Fatal(err)
    }

    commentQuery := `CREATE TABLE IF NOT EXISTS comments (
        id SERIAL PRIMARY KEY,
        meme_id INTEGER REFERENCES memes(id) ON DELETE CASCADE,
        author TEXT NOT NULL,
        content TEXT NOT NULL,
        created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
    )`
    _, err = db.Exec(commentQuery)
    if err != nil {
        log.Fatal(err)
    }
}

// Fetch all memes
func getAllMemes() []Meme {
    rows, err := db.Query("SELECT id, template, lines, url, context, link, votes FROM memes ORDER BY votes DESC")
    if err != nil {
        log.Println("Error fetching memes:", err)
        return nil
    }
    defer rows.Close()

    var memes []Meme
    for rows.Next() {
        var meme Meme
        var linesJSON string

        err := rows.Scan(&meme.ID, &meme.Template, &linesJSON, &meme.URL, &meme.Context, &meme.Link, &meme.Votes)
        if err != nil {
            log.Println("Error scanning meme:", err)
            continue
        }

        // Decode JSON lines
        var lines []string
        json.Unmarshal([]byte(linesJSON), &lines)
        meme.Lines = strings.Join(lines, "\n") // Convert to a readable format

        // Fetch comments for this meme
        meme.Comments = getCommentsForMeme(meme.ID)

        memes = append(memes, meme)
    }
    return memes
}

// Save meme metadata
func saveMeme(template, linesJSON, url, context, link string) {
    _, err := db.Exec("INSERT INTO memes (template, lines, url, context, link, votes) VALUES ($1, $2, $3, $4, $5, 0)",
        template, linesJSON, url, context, link)
    if err != nil {
        log.Println("Error inserting meme:", err)
    }
}

// Serve meme via Redis
func serveMeme(c *fiber.Ctx) error {
    id := c.Params("id")

    // Fetch meme from DB
    var memeURL string
    err := db.QueryRow("SELECT url FROM memes WHERE id = $1", id).Scan(&memeURL)
    if err != nil {
        return c.Status(404).SendString("Meme not found")
    }

    // Check if meme is cached in Redis
    cacheKey := fmt.Sprintf("meme:%s", id)
    cachedImage, err := redisClient.Get(ctx, cacheKey).Bytes()
    if err == nil {
        c.Set("Content-Type", "image/png")
        return c.Send(cachedImage)
    }

    // Fetch meme from API
    resp, err := http.Get(memeURL)
    if err != nil {
        return c.Status(500).SendString("Error fetching meme image")
    }
    defer resp.Body.Close()

    // Read image into memory
    imageBytes, err := io.ReadAll(resp.Body)
    if err != nil {
        return c.Status(500).SendString("Error processing meme image")
    }

    // Cache image in Redis for 7 days
    redisClient.Set(ctx, cacheKey, imageBytes, 604800*time.Second)

    // Serve the image
    c.Set("Content-Type", "image/png")
    return c.Send(imageBytes)
}

// Fetch Memegen templates
func fetchMemegenTemplates() ([]MemeTemplate, error) {
    templates, err := getTemplatesFromRedis()
    if err != nil || len(templates) == 0 {
        log.Println("Fetching templates from API (Redis cache miss)")
        return fetchTemplatesFromAPI()
    }
    for _, t := range templates {
      log.Printf("Injecting Template: %s, Lines: %d", t.ID, t.Lines)
    }
    return templates, nil
}

// Fetch from Redis
func getTemplatesFromRedis() ([]MemeTemplate, error) {
    ctx := context.Background()
    templatesJSON, err := redisClient.Get(ctx, "memegen-templates").Result()
    if err != nil {
        return nil, err // Redis miss
    }

    var templates []MemeTemplate
    err = json.Unmarshal([]byte(templatesJSON), &templates)
    if err != nil {
        return nil, err
    }

    return templates, nil
}

// Fetch from API
func fetchTemplatesFromAPI() ([]MemeTemplate, error) {
    resp, err := http.Get(memegenAPI + "/templates")
    if err != nil {
        log.Println("Error fetching templates from API:", err)
        return nil, err
    }
    defer resp.Body.Close()

    if resp.StatusCode != http.StatusOK {
        log.Println("Memegen API returned non-200 status:", resp.Status)
        return nil, fmt.Errorf("failed to fetch templates: %s", resp.Status)
    }

    body, err := io.ReadAll(resp.Body)
    if err != nil {
        log.Println("Error reading API response:", err)
        return nil, err
    }

    var templates []MemeTemplate
    err = json.Unmarshal(body, &templates)
    if err != nil {
        log.Println("Error parsing templates JSON:", err)
        return nil, err
    }

    // Store templates in Redis with expiration (e.g., 24 hours)
    storeTemplatesInRedis(templates)

    return templates, nil
}

func storeTemplatesInRedis(templates []MemeTemplate) {
    ctx := context.Background()
    templatesJSON, err := json.Marshal(templates)
    if err != nil {
        log.Println("Error marshaling templates:", err)
        return
    }

    err = redisClient.Set(ctx, "memegen-templates", templatesJSON, 24*time.Hour).Err()
    if err != nil {
        log.Println("Error storing templates in Redis:", err)
    } else {
        log.Println("Successfully cached templates in Redis for 24 hours")
    }
}

// Render HTML template
func renderTemplate(c *fiber.Ctx, templateName string, data interface{}) error {
    tmpl, err := template.ParseFiles("templates/" + templateName)
    if err != nil {
        log.Println("Error parsing template:", err)
        return c.Status(500).SendString("Template rendering error")
    }
    c.Set("Content-Type", "text/html; charset=utf-8")
    return tmpl.Execute(c.Response().BodyWriter(), data)
}

// Generate meme on demand
func generateMeme(template, topText, bottomText string) ([]byte, error) {
    url := fmt.Sprintf("%s/images/%s/%s/%s.png", memegenAPI, template, topText, bottomText)
    resp, err := http.Get(url)
    if err != nil {
        return nil, err
    }
    defer resp.Body.Close()

    return io.ReadAll(resp.Body)
}


// Get meme parameters from SQLite DB
func getMemeParams(url string) (template, topText, bottomText string) {
    row := db.QueryRow("SELECT template, topText, bottomText FROM memes WHERE url = $1", url)
    err := row.Scan(&template, &topText, &bottomText)
    if err != nil {
        log.Println("Error retrieving meme params:", err)
    }
    return
}

// Get current meme votes
func getMemeVotes(memeID string) int {
    var votes int
    err := db.QueryRow("SELECT votes FROM memes WHERE id = $1", memeID).Scan(&votes)
    if err != nil {
        log.Println("Error fetching meme votes:", err)
        return 0
    }
    return votes
}

// Get comments for a specific meme
func getCommentsForMeme(memeID int) []Comment {
    rows, err := db.Query("SELECT id, meme_id, author, content, created_at FROM comments WHERE meme_id = $1 ORDER BY created_at ASC", memeID)
    if err != nil {
        log.Println("Error fetching comments:", err)
        return nil
    }
    defer rows.Close()

    var comments []Comment
    for rows.Next() {
        var comment Comment
        err := rows.Scan(&comment.ID, &comment.MemeID, &comment.Author, &comment.Content, &comment.CreatedAt)
        if err != nil {
            log.Println("Error scanning comment:", err)
            continue
        }
        comments = append(comments, comment)
    }
    return comments
}
