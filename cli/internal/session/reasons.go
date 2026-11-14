package session

import "github.com/idestis/knock/protocol"

// Explain turns an end reason into a sentence for the person at this end.
func Explain(reason, role string, peer protocol.Identity) string {
	p := peerFirstName(peer)
	switch reason {
	case ReasonYouEnded:
		if role == protocol.RoleGuest {
			return "You left."
		}
		return "You ended the session."
	case ReasonShellExited: // same string locally and from the relay
		if role == protocol.RoleHost {
			return "The shell exited, so the session ended."
		}
		return p + "'s shell exited, so the session ended."
	case protocol.EndHostQuit:
		if role == protocol.RoleHost {
			return "You quit Knock, so the session ended."
		}
		return p + " quit Knock, so the session ended."
	case ReasonRelayLost:
		return "Lost the connection to the relay, so the session ended."
	case "host_ended":
		if role == protocol.RoleHost {
			return "You ended the session."
		}
		return p + " ended the session."
	case "guest_left":
		if role == protocol.RoleGuest {
			return "You left."
		}
		return p + " left."
	case "host_left":
		return p + "'s connection dropped, so the session ended."
	case "code_changed":
		if role == protocol.RoleHost {
			return "Your code changed, so the session ended."
		}
		return p + " changed their code, so the session ended. Ask them for the new one to knock again."
	case "door_closed":
		if role == protocol.RoleHost {
			return "You closed your door, so the session ended."
		}
		return p + " closed their door."
	case "opened_elsewhere":
		if role == protocol.RoleHost {
			return "Your door was opened somewhere else, so this session ended."
		}
		return p + " opened their door somewhere else, so the session ended."
	case "":
		return "The session ended."
	}
	return "The session ended (" + reason + ")."
}
