// Package model defines the shared wire types for the session-gateway abstraction layer.
//
// The type hierarchy:
//
//	Envelope                    { EntityType, EventType }
//	└── SessionBase             { +SessionID }
//	    └── TurnBase            { +TurnID }
//
// Envelope is passed by value (2 strings). All other structs are passed by pointer.
//
// Every message on the wire — whether a backend→client event or a client→backend
// request — carries entity_type + event_type as a routing pair.
package model
