package coolify

import (
	"context"
	"database/sql"
	"errors"
	"net"
	"net/url"
	"strconv"
	"time"

	"github.com/JH0project/fabric/control/internal/database"
	"github.com/JH0project/fabric/control/internal/environment"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	"gorm.io/gorm"
)

type Config struct {
	Host, Port, Name, Username, Password, CA, Certificate, Key string
	AppKeys                                                    []string
	Cipher                                                     string
	ServerID                                                   int64
}

// Store owns only reads from the existing Coolify schema; it never migrates it.
type Store struct {
	database  *database.Store
	decryptor *decryptor
	serverID  int64
}

func Open(ctx context.Context, c Config) (*Store, error) {
	if c.Host == "" || c.Name == "" || c.Username == "" || c.Password == "" || c.ServerID < 0 {
		return nil, errors.New("Coolify database configuration incomplete")
	}
	if c.Port == "" {
		c.Port = "5432"
	}
	port, err := strconv.Atoi(c.Port)
	if err != nil || port < 1 || port > 65535 {
		return nil, errors.New("Coolify database port invalid")
	}
	if c.Cipher == "" {
		c.Cipher = "AES-256-CBC"
	}
	d, err := newDecryptor(c.AppKeys, c.Cipher)
	if err != nil {
		return nil, err
	}
	u := url.URL{Scheme: "postgresql", Host: net.JoinHostPort(c.Host, c.Port), Path: "/" + c.Name, User: url.UserPassword(c.Username, c.Password)}
	q := url.Values{"sslmode": {"verify-full"}, "sslrootcert": {c.CA}, "default_transaction_read_only": {"on"}, "search_path": {"public"}}
	if c.Certificate != "" {
		q.Set("sslcert", c.Certificate)
	}
	if c.Key != "" {
		q.Set("sslkey", c.Key)
	}
	u.RawQuery = q.Encode()
	db, err := database.Open(ctx, u.String())
	if err != nil {
		return nil, errors.New("Coolify database connection failed")
	}
	return &Store{db, d, c.ServerID}, nil
}
func (s *Store) Close() error { return s.database.Close() }

func (s *Store) Load(ctx context.Context, ref environment.Reference) (result environment.Snapshot, err error) {
	ctx, span := otel.Tracer("github.com/JH0project/fabric/control/coolify").Start(ctx, "coolify.environment.read")
	defer func() {
		if err != nil {
			span.SetStatus(codes.Error, "Coolify environment read failed")
		}
		span.End()
	}()
	if !ref.Valid() || ref.ServerID != s.serverID {
		return result, environment.ErrUnavailable
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	err = s.database.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var app struct{ ID, TeamID, ProjectID, EnvironmentID int64 }
		// Authorization is one round trip even for additional destinations.
		query := tx.Table("public.applications AS a").
			Select("a.id,a.environment_id,e.project_id,p.team_id").
			Joins("JOIN public.environments e ON e.id=a.environment_id").
			Joins("JOIN public.projects p ON p.id=e.project_id").
			Joins("JOIN public.servers target ON target.id=? AND target.deleted_at IS NULL", ref.ServerID).
			Joins("LEFT JOIN public.standalone_dockers d ON a.destination_type=? AND d.id=a.destination_id", `App\Models\StandaloneDocker`).
			Joins("LEFT JOIN public.swarm_dockers sw ON a.destination_type=? AND sw.id=a.destination_id", `App\Models\SwarmDocker`).
			Joins("JOIN public.servers primary_server ON primary_server.id=COALESCE(d.server_id,sw.server_id) AND primary_server.deleted_at IS NULL").
			Where("a.uuid=? AND a.deleted_at IS NULL AND p.team_id=? AND target.team_id=p.team_id AND primary_server.team_id=p.team_id", ref.ApplicationUUID, ref.TeamID).
			Where("(primary_server.id=target.id OR EXISTS (SELECT 1 FROM public.additional_destinations ad WHERE ad.application_id=a.id AND ad.server_id=target.id))").Take(&app)
		if query.Error != nil {
			return environment.ErrUnavailable
		}
		result = environment.Snapshot{TeamID: app.TeamID, ProjectID: app.ProjectID, EnvironmentID: app.EnvironmentID, ServerID: ref.ServerID}
		var vars []struct {
			Key                  string
			Value                *string
			IsRuntime, IsPreview bool
		}
		if tx.Table("public.environment_variables").Select("key,value,is_runtime,is_preview").Where("resourceable_type=? AND resourceable_id=? AND is_runtime=true AND is_preview=false", `App\Models\Application`, app.ID).Order("id").Limit(8193).Find(&vars).Error != nil || len(vars) > 8192 {
			return environment.ErrUnavailable
		}
		for _, v := range vars {
			if environment.BuildOnly(v.Key) {
				continue
			}
			text := ""
			if v.Value != nil {
				var e error
				text, e = s.decryptor.decrypt(*v.Value, true)
				if e != nil {
					return e
				}
			}
			result.Variables = append(result.Variables, environment.ApplicationVariable{Variable: environment.Variable{Key: v.Key, Value: text}, Runtime: v.IsRuntime, Preview: v.IsPreview})
		}
		references := environment.SharedReferences(result.Variables)
		if len(references) == 0 {
			return nil
		}
		var shared []struct {
			Key, Type                          string
			Value                              *string
			TeamID                             int64
			ProjectID, EnvironmentID, ServerID *int64
		}
		if tx.Table("public.shared_environment_variables").Select("key,value,type,team_id,project_id,environment_id,server_id").Where("team_id=? AND ((type='team') OR (type='project' AND project_id=?) OR (type='environment' AND environment_id=?) OR (type='server' AND server_id=?))", ref.TeamID, app.ProjectID, app.EnvironmentID, ref.ServerID).Order("id").Limit(8193).Find(&shared).Error != nil || len(vars)+len(shared) > 8192 {
			return environment.ErrUnavailable
		}
		for _, v := range shared {
			if !references[v.Type+"."+v.Key] {
				continue
			}
			id := v.TeamID
			switch v.Type {
			case "project":
				if v.ProjectID == nil {
					continue
				}
				id = *v.ProjectID
			case "environment":
				if v.EnvironmentID == nil {
					continue
				}
				id = *v.EnvironmentID
			case "server":
				if v.ServerID == nil {
					continue
				}
				id = *v.ServerID
			}
			text := ""
			if v.Value != nil {
				var e error
				text, e = s.decryptor.decrypt(*v.Value, false)
				if e != nil {
					return e
				}
			}
			result.Shared = append(result.Shared, environment.SharedVariable{Variable: environment.Variable{Key: v.Key, Value: text}, Scope: v.Type, ScopeID: id})
		}
		return nil
	}, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return environment.Snapshot{}, environment.ErrUnavailable
	}
	return result, nil
}
