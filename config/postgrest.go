package config

import (
	"fmt"
	"strings"

	"github.com/spf13/viper"
)

// miniship (miniship-cloud#1302, public ADR 0077): rest answers the Supabase
// SDK's database client on /rest/v1/<table>, in PostgREST's language. The path
// names no Project, so where it comes from, and where a User's key set is read,
// are two settings of their own.

// PostgRESTConf is what the /rest/v1 address needs beyond the table read's own
// settings.
type PostgRESTConf struct {
	// ProjectHeader is the request header that names the Project a call is
	// for, and it is read only when this is set. It is set where rest sits
	// behind a gateway that removes a caller's own copy of the header and
	// writes its own: miniship cloud, whose Check names the Project from the
	// address the call arrived at. Empty is a self-host install, where the
	// header is anybody's to type and rest serves the install's one Project.
	ProjectHeader string `mapstructure:"project_header"`
	// ProjectKeysURL is where a Project's published key set is read, the keys
	// its User pool signs a User's token with. It holds {project} once, where
	// the Project's id goes: the Project is named in the path because a
	// header would not survive every gateway in front of the auth api. Empty
	// is rest refusing every User's token, 401, as it did before.
	ProjectKeysURL string `mapstructure:"project_keys_url"`
}

// ProjectPlaceholder is where the Project's id goes in ProjectKeysURL.
const ProjectPlaceholder = "{project}"

// ProjectKeysURLFor is the key-set address of one Project.
func (c PostgRESTConf) ProjectKeysURLFor(project string) string {
	return strings.Replace(c.ProjectKeysURL, ProjectPlaceholder, project, 1)
}

// parsePostgREST reads the two settings, unprefixed as the lookup's are
// (PROJECT_HEADER beside PREST_PROJECT_HEADER), so the compose file writes one
// prefix for every miniship setting of the service.
func parsePostgREST(v *viper.Viper, cfg *Prest) error {
	cfg.PostgREST = PostgRESTConf{
		ProjectHeader:  v.GetString("postgrest.project_header"),
		ProjectKeysURL: v.GetString("postgrest.project_keys_url"),
	}
	if header := envFirst("PROJECT_HEADER", "PREST_PROJECT_HEADER"); header != "" {
		cfg.PostgREST.ProjectHeader = header
	}
	if keys := envFirst("PROJECT_KEYS_URL", "PREST_PROJECT_KEYS_URL"); keys != "" {
		cfg.PostgREST.ProjectKeysURL = keys
	}
	return cfg.PostgREST.validate()
}

// validate refuses a key-set address with no place for the Project: every
// Project would be asked about at one address, and one Project's keys would
// open another's rows.
func (c PostgRESTConf) validate() error {
	if c.ProjectKeysURL != "" && strings.Count(c.ProjectKeysURL, ProjectPlaceholder) != 1 {
		return fmt.Errorf("PROJECT_KEYS_URL holds %s exactly once, where the Project's id goes", ProjectPlaceholder)
	}
	return nil
}
