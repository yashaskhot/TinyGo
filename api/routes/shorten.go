package routes

import (
	"fmt"
	"os"
	"strconv"
	"time"

	"tinygo/database"
	"tinygo/helpers"

	"github.com/asaskevich/govalidator"
	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

type request struct {
	URL         string        `json:"url"`
	CustomShort string        `json:"short"`
	Expiry      time.Duration `json:"expiry"`
}

type response struct {
	URL             string        `json:"url"`
	CustomShort     string        `json:"short"`
	Expiry          time.Duration `json:"expiry"`
	XRateRemaining  int           `json:"rate_limit"`
	XRateLimitReset time.Duration `json:"rate_limit_reset"`
}

// ShortenURL ...
func ShortenURL(c *fiber.Ctx) error {
	// check for the incoming request body
	body := new(request)
	if err := c.BodyParser(&body); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"error": "cannot parse JSON",
		})
	}

	// check if the input is an actual URL
	if !govalidator.IsURL(body.URL) {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"error": "invalid URL",
		})
	}

	// check for the domain error
	if !helpers.RemoveDomainError(body.URL) {
		return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{
			"error": "haha... nice try",
		})
	}

	// enforce https
	body.URL = helpers.EnforceHTTP(body.URL)

	// check if the user has provided any custom dhort urls
	var id string
	if body.CustomShort == "" {
		id = uuid.New().String()[:6]
	} else {
		id = body.CustomShort
	}

	r := database.CreateClient(0)
	defer r.Close()

	val, _ := r.Get(database.Ctx, id).Result()
	// check if the user provided short is already in use
	if val != "" {
		return c.Status(fiber.StatusForbidden).JSON(fiber.Map{
			"error": "URL short already in use",
		})
	}

	// implement rate limiting
	quota, err := strconv.Atoi(os.Getenv("API_QUOTA"))
	if err != nil {
		quota = 100 // default quota
	}
	remaining, exp, err := handleRateLimit(r, c.IP(), quota)
	if err != nil {
		return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{
			"error":            err.Error(),
			"rate_limit_reset": exp / time.Second / time.Minute,
		})
	}

	if body.Expiry == 0 {
		body.Expiry = 24 // default expiry of 24 hours
	}
	err = r.Set(database.Ctx, id, body.URL, body.Expiry*3600*time.Second).Err()
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"error": "unable to connect to server",
		})
	}

	// respond with the url, short, expiry in hours, calls remaining and time to reset
	resp := response{
		URL:             body.URL,
		CustomShort:     os.Getenv("DOMAIN") + "/" + id,
		Expiry:          body.Expiry,
		XRateRemaining:  remaining,
		XRateLimitReset: exp / time.Nanosecond / time.Minute,
	}

	return c.Status(fiber.StatusOK).JSON(resp)
}

func handleRateLimit(r *redis.Client, ip string, quota int) (int, time.Duration, error) {
	// Get the current rate limit value for the IP
	val, err := r.Get(database.Ctx, ip).Result()
	if err == redis.Nil {
		// If the IP is not found, set the initial rate limit quota and expiry
		err = r.Set(database.Ctx, ip, quota, 30*60*time.Second).Err()
		if err != nil {
			return 0, 0, err
		}
		return quota, 30 * time.Minute, nil
	} else if err != nil {
		return 0, 0, err
	}

	// If the IP is found, check if the rate limit has been exceeded
	remaining, err := strconv.Atoi(val)
	if err != nil {
		return 0, 0, err
	}
	if remaining <= 0 {
		// If the rate limit has been exceeded, return the remaining time until reset
		ttl, err := r.TTL(database.Ctx, ip).Result()
		if err != nil {
			return 0, 0, err
		}
		return 0, ttl, fmt.Errorf("rate limit exceeded")
	}

	// Decrement the rate limit value and update the expiry time
	err = r.Decr(database.Ctx, ip).Err()
	if err != nil {
		return 0, 0, err
	}
	err = r.Expire(database.Ctx, ip, 30*60*time.Second).Err()
	if err != nil {
		return 0, 0, err
	}

	return remaining - 1, 30 * time.Minute, nil
}
