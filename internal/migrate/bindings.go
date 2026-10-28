package migrate

import (
	"crypto/sha256"
	"fmt"
	"strconv"
	"strings"
)

func explicitEnv(value string) string {
	if m := wholeEnvReference.FindStringSubmatch(value); m != nil {
		return m[1]
	}
	if strings.HasPrefix(value, "${") && strings.HasSuffix(value, "}") {
		name := strings.TrimSuffix(strings.TrimPrefix(value, "${"), "}")
		if wholeEnvReference.MatchString("$" + name) {
			return name
		}
	}
	return ""
}

func (g *generator) envBinding(name string) string {
	g.imports["os"] = true
	g.imports["strings"] = true
	g.helpers["environment"] = `func migrationEnv(name string) string {
 value,ok:=os.LookupEnv(name)
 if !ok || value=="" {panic("missing required migration environment binding: "+name)}
 if strings.ContainsAny(value,"\r\n\x00") {panic("invalid migration environment binding: "+name)}
 return value
}`
	g.r.Report.Checks = append(g.r.Report.Checks, Check{Tool: toolBindings, Code: "environment-required:" + name, Deferred: true})
	return "migrationEnv(" + strconv.Quote(name) + ")"
}

// The audit never evaluates a reference. Its validation value is a placeholder;
// the report distinguishes structural checking from material validation.
func (g *generator) sensitiveValue(o *Object, path, value string) (string, string) {
	name := explicitEnv(value)
	if name == "" {
		sum := sha256.Sum256([]byte(o.identity() + ":" + path))
		name = fmt.Sprintf("STATUTE_MIGRATION_%X", sum[:6])
		o.mark(path, Manual, "inline value withheld; move it into the generated environment binding before deployment")
	} else {
		o.mark(path, Translated, "explicit environment reference retained; required binding checked before startup")
	}
	return "migration-binding", g.envBinding(name)
}

func (g *generator) usersFile(path string) string {
	g.imports["os"] = true
	g.imports["strings"] = true
	g.helpers["users-file"] = `func migrationUsersFile(path string) map[string]string {
 data,err:=os.ReadFile(path)
 if err!=nil {panic("cannot read migration authentication file")}
 users:=map[string]string{}
 for _,line:=range strings.Split(string(data),"\n") {
  line=strings.TrimSpace(line)
  if line=="" || strings.HasPrefix(line,"#") {continue}
  name,hash,ok:=strings.Cut(line,":")
  if !ok || name=="" || hash=="" {panic("invalid migration authentication file")}
  if _,exists:=users[name];exists {panic("duplicate migration authentication user")}
  users[name]=hash
 }
 if len(users)==0 {panic("empty migration authentication file")}
 return users
}`
	g.r.Report.Checks = append(g.r.Report.Checks, Check{Tool: toolBindings, Code: "bcrypt-authentication-file-required", Deferred: true})
	return "migrationUsersFile(" + strconv.Quote(path) + ")"
}
