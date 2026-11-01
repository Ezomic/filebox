package auth

import (
	"encoding/json"
	"errors"
	"log"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

const contactsFile = "contacts.json"

// Contact is a portal user's own email address. It is only used for reset once the user has confirmed it.
type Contact struct {
	Email     string `json:"email"`
	Confirmed bool   `json:"confirmed"`
}

type contactsDisk struct {
	Users map[string]Contact `json:"users"`
}

// Contacts keeps addresses next to roles.json because Linux accounts have no field for them.
// An address is stored as the user typed it and compared case-insensitively.
type Contacts struct {
	path string

	mu     sync.RWMutex
	byUser map[string]Contact
}

func OpenContacts(dir string) (*Contacts, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}

	contacts := &Contacts{path: filepath.Join(dir, contactsFile), byUser: map[string]Contact{}}
	content, err := os.ReadFile(contacts.path)
	if errors.Is(err, os.ErrNotExist) {
		return contacts, nil
	}
	if err != nil {
		return nil, err
	}

	var loaded contactsDisk
	if err := json.Unmarshal(content, &loaded); err != nil {
		log.Printf("auth: %s is damaged (%v), starting with no addresses", contacts.path, err)
		if err := os.Rename(contacts.path, contacts.path+".bad"); err != nil {
			log.Printf("auth: could not keep the damaged file as %s.bad: %v", contacts.path, err)
		}

		return contacts, nil
	}
	if loaded.Users != nil {
		contacts.byUser = loaded.Users
	}

	return contacts, nil
}

func (c *Contacts) Get(user string) (Contact, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	contact, found := c.byUser[user]

	return contact, found
}

// Set stores an address as unconfirmed, replacing any earlier address and its confirmation.
func (c *Contacts) Set(user, email string) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	next := maps.Clone(c.byUser)
	next[user] = Contact{Email: email}

	return c.save(next)
}

// Confirm succeeds only while the stored address is still the one the link was sent to and no other
// account has confirmed it, so a confirmed address always identifies one account.
func (c *Contacts) Confirm(user, email string) (bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	contact, found := c.byUser[user]
	if !found || !strings.EqualFold(contact.Email, email) {
		return false, nil
	}
	if contact.Confirmed {
		return true, nil
	}
	if c.confirmedBy(email, user) {
		return false, nil
	}
	next := maps.Clone(c.byUser)
	contact.Confirmed = true
	next[user] = contact
	if err := c.save(next); err != nil {
		return false, err
	}

	return true, nil
}

// Lookup finds the account whose confirmed address this is. An address that old data lists as confirmed
// on more than one account resolves to nobody.
func (c *Contacts) Lookup(email string) (string, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	owner, count := "", 0
	for user, contact := range c.byUser {
		if contact.Confirmed && strings.EqualFold(contact.Email, email) {
			owner = user
			count++
		}
	}

	return owner, count == 1
}

// confirmedBy reports whether an account other than skip has confirmed the address. Callers hold the lock.
func (c *Contacts) confirmedBy(email, skip string) bool {
	for user, contact := range c.byUser {
		if user != skip && contact.Confirmed && strings.EqualFold(contact.Email, email) {
			return true
		}
	}

	return false
}

func (c *Contacts) Forget(user string) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if _, found := c.byUser[user]; !found {
		return nil
	}
	next := maps.Clone(c.byUser)
	delete(next, user)

	return c.save(next)
}

func (c *Contacts) save(next map[string]Contact) error {
	content, err := json.Marshal(contactsDisk{Users: next})
	if err != nil {
		return err
	}
	if err := writeFileAtomic(c.path, content); err != nil {
		return err
	}
	c.byUser = next

	return nil
}
