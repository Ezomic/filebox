package server

type mailLang uint8

const (
	mailEnglish mailLang = iota
	mailDutch
)

// The portal sends the language its visitor picked. It only chooses between two fixed templates,
// so anything that is not exactly the string "nl" gets English.
func chosenMailLang(value any) mailLang {
	if code, ok := value.(string); ok && code == "nl" {
		return mailDutch
	}

	return mailEnglish
}

type mailText struct {
	subject string
	body    string
}

func (l mailLang) confirmation(user, link string) mailText {
	if l == mailDutch {
		return mailText{
			subject: "Bevestig je e-mailadres voor Filebox",
			body: "Hallo,\n\nJe wilt dit adres gebruiken om het wachtwoord van het account \"" + user +
				"\" op Filebox opnieuw in te stellen. Open binnen een uur deze link om het te bevestigen:\n\n" + link +
				"\n\nDe link werkt maar een keer. Heb je dit niet aangevraagd, negeer deze e-mail dan. Er verandert niets.\n",
		}
	}

	return mailText{
		subject: "Confirm your email address for Filebox",
		body: "Hello,\n\nYou asked to use this address for password resets of the Filebox account \"" + user +
			"\". Open this link within one hour to confirm it:\n\n" + link +
			"\n\nThe link works once. If you did not ask for this, ignore this email and nothing changes.\n",
	}
}

func (l mailLang) reset(user, link string) mailText {
	if l == mailDutch {
		return mailText{
			subject: "Wachtwoord opnieuw instellen voor Filebox",
			body: "Hallo,\n\nIemand heeft gevraagd om het wachtwoord van het account \"" + user +
				"\" op Filebox opnieuw in te stellen. Open binnen 30 minuten deze link om een nieuw wachtwoord te kiezen:\n\n" + link +
				"\n\nDe link werkt maar een keer. Heb je dit niet aangevraagd, negeer deze e-mail dan. Je wachtwoord blijft zoals het is.\n",
		}
	}

	return mailText{
		subject: "Reset your Filebox password",
		body: "Hello,\n\nSomeone asked to reset the password of the Filebox account \"" + user +
			"\". Open this link within 30 minutes to choose a new password:\n\n" + link +
			"\n\nThe link works once. If you did not ask for this, ignore this email and your password stays as it is.\n",
	}
}

func (l mailLang) signIn(user, link string) mailText {
	if l == mailDutch {
		return mailText{
			subject: "Je inloglink voor Filebox",
			body: "Hallo,\n\nIemand heeft gevraagd om een link om in te loggen op het account \"" + user +
				"\" op Filebox. Open binnen 15 minuten deze link en druk op de knop om in te loggen:\n\n" + link +
				"\n\nDe link werkt maar een keer en verandert je wachtwoord niet. Heb je dit niet aangevraagd, negeer deze e-mail dan.\n",
		}
	}

	return mailText{
		subject: "Your Filebox sign-in link",
		body: "Hello,\n\nSomeone asked for a link to sign in to the Filebox account \"" + user +
			"\". Open this link within 15 minutes and press the button to sign in:\n\n" + link +
			"\n\nThe link works once and does not change your password. If you did not ask for this, ignore this email.\n",
	}
}
