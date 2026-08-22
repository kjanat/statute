# GitHub milestone commands

Use these commands when inspecting Statute's milestone queue. They are display
and navigation helpers; `/next` selection must still sort live open milestones
by numeric `.number`, not by due date or display order.

Do not install or overwrite aliases unless the user explicitly asks. If the
aliases already exist, they may be called directly. Quote endpoints containing
`{owner}`, `{repo}`, or query parameters so the shell does not expand them.

## `gh ms`: compact milestone table

```sh
gh alias set ms - <<'EOF'
api "/repos/{owner}/{repo}/milestones?state=all&sort=due_on&direction=asc" --paginate --template '
{{- tablerow "ID" "MILESTONE" "OPEN" "CLOSED" "DUE" "UPDATED" -}}
{{- range . -}}
  {{- $due := "-" -}}{{- if .due_on -}}{{- $due = timefmt "2006-01-02" .due_on -}}{{- end -}}
  {{- $id := printf "#%v" .number -}}
  {{- if eq .state "open" -}}{{- $id = autocolor "green" $id -}}{{- else -}}{{- $id = autocolor "magenta" $id -}}{{- end -}}
  {{- tablerow $id (hyperlink .html_url (truncate 45 .title)) (printf "%v" .open_issues) (printf "%v" .closed_issues) $due (timeago .updated_at) -}}
{{- end -}}
{{- tablerender -}}'
EOF
```

The title is a terminal hyperlink. This alias intentionally sorts by due date
for display; do not use its row order as `/next` priority.

## Milestone detail view

```sh
gh api '/repos/{owner}/{repo}/milestones' --template '
{{- range . -}}
{{- $due := "no due date" -}}{{- if .due_on -}}{{- $due = timefmt "Jan 2, 2006" .due_on -}}{{- end -}}
{{ autocolor "green+b" (printf "#%v" .number) }} {{ hyperlink .html_url .title }} {{ autocolor "yellow" (printf "(%v)" .state) }}
{{ if .description }}   {{ .description }}
{{ end }}   {{ .open_issues }} open · {{ .closed_issues }} closed · {{ $due }} · updated {{ timeago .updated_at }}

{{ end -}}'
```

## Milestone progress bars

Go templates have no arithmetic, so this view uses `--jq`. `gh api` does not
accept `--jq` and `--template` together.

```sh
gh api '/repos/{owner}/{repo}/milestones' --paginate --jq '
  def rpad($n): .[:$n] + ("                                                  "[:($n - (.[:$n]|length))]);
  def lpad($n): ("     "[:($n-length)] // "") + .;
  def bar($p): ($p/5|floor) as $f | "[" + ("█"*$f // "") + ("░"*(20-$f) // "") + "]";
  .[] | (.open_issues + .closed_issues) as $t
  | (if $t == 0 then 0 else (.closed_issues*100/$t) end) as $p
  | "\("#\(.number)"|rpad(4)) \(.title|rpad(38)) \(bar($p)) \($p|floor|tostring|lpad(3))%  \(.closed_issues)/\($t)"'
```

The `// ""` guards are required because multiplying a jq string by zero yields
`null`.

## `gh mi <number-or-title>`: one milestone's issues

```sh
gh alias set mi - <<'EOF'
issue list --milestone "$1" --state all --limit 100 --json number,title,state,url,updatedAt,labels,assignees --template '
{{- tablerow "ID" "TITLE" "LABELS" "ASSIGNEE" "UPDATED" -}}
{{- range . -}}
  {{- $id := printf "#%v" .number -}}
  {{- if eq .state "OPEN" -}}{{- $id = autocolor "green" $id -}}{{- else -}}{{- $id = autocolor "magenta" $id -}}{{- end -}}
  {{- $who := "-" -}}{{- if .assignees -}}{{- $who = (.assignees | pluck "login" | join ",") -}}{{- end -}}
  {{- $lbl := "-" -}}{{- if .labels -}}{{- $lbl = (.labels | pluck "name" | join ", " | autocolor "yellow") -}}{{- end -}}
  {{- tablerow $id (hyperlink .url (truncate 55 .title)) $lbl $who (timeago .updatedAt) -}}
{{- end -}}
{{- tablerender -}}'
EOF
```

`--milestone` accepts its number or exact title. Additional issue-list flags
append normally, for example `gh mi 1 --label bug`.

## `gh mb`: every open milestone with nested issues

This uses one GraphQL request rather than one request per milestone.

```sh
gh alias set mb - <<'EOF'
api graphql -F owner='{owner}' -F name='{repo}' -f query='
  query($owner: String!, $name: String!) {
    repository(owner: $owner, name: $name) {
      milestones(first: 20, states: [OPEN], orderBy: {field: DUE_DATE, direction: ASC}) {
        nodes {
          number title dueOn
          issues(first: 50, states: [OPEN], orderBy: {field: CREATED_AT, direction: ASC}) {
            nodes {
              number title url
              labels(first: 5) { nodes { name } }
              assignees(first: 3) { nodes { login } }
            }
          }
        }
      }
    }
  }
' --template '
{{- range .data.repository.milestones.nodes -}}
{{- $due := "" -}}{{- if .dueOn -}}{{- $due = printf " · due %s" (timefmt "2006-01-02" .dueOn) -}}{{- end -}}
{{ autocolor "green+b" (printf "#%v %s" .number .title) }}{{ autocolor "gray" $due }}
{{ if not .issues.nodes }}   (no open issues)
{{ else }}{{ range .issues.nodes -}}
{{- $who := "-" -}}{{- if .assignees.nodes -}}{{- $who = (.assignees.nodes | pluck "login" | join ",") -}}{{- end -}}
{{- $lbl := "-" -}}{{- if .labels.nodes -}}{{- $lbl = (.labels.nodes | pluck "name" | join ", " | autocolor "yellow") -}}{{- end -}}
{{- tablerow (printf "   #%v" .number) (hyperlink .url (truncate 55 .title)) $lbl $who -}}
{{- end }}{{ tablerender }}{{ end }}
{{ end -}}'
EOF
```

The nested `tablerender` resets alignment for each milestone. This query is
bounded at 20 milestones and 50 issues per milestone; add cursor pagination for
larger repositories. It includes issues only, not pull requests. Its milestone
order is due date, so `/next` must independently sort by `.number`.

Use `autocolor`, not `color`, so piped output has no unwanted ANSI sequences.
The REST milestone state is lowercase, while `gh issue list --json state` uses
uppercase GraphQL values.
