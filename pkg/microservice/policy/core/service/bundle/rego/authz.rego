package rbac

import input.attributes.request.http as http_request

# Policy rule definitions in rbac style which is consumed by OPA server.
# you can use it to:
# 1. decide if a request is allowed by querying: rbac.allow
# 2. get all visible projects for an authenticated user by querying: rbac.user_visible_projects
# 3. get all allowed projects for a certain action(method+endpoint) for an authenticated user by querying: rbac.user_allowed_projects
# 4. check if a user is system admin by querying: rbac.user_is_admin
# 5. check if a user is project admin by querying: rbac.user_is_project_admin


# By default, deny requests.
default allow = false

# Allow everyone to visit public urls.
allow {
    url_is_public
}

# Allow all valid users to visit exempted urls.
allow {
    url_is_exempted
    claims.name != ""
}

# Allow admins to do anything.
allow {
    user_is_admin
}

# Allow project admins to do anything under the given project.
allow {
    user_is_project_admin
}

# Allow the action if the user is granted permission to perform the action.
allow {
    some grant
    user_is_granted[grant]

    grant.method == http_request.method
    glob.match(trim(grant.endpoint, "/"), ["/"], concat("/", input.parsed_path))
}

user_is_admin {
    some role
    all_roles[role]

    role.name == "admin"
    role.namespace == ""
}

user_is_project_admin {
    some role
    allowed_roles[role]

    role.name == "admin"
    role.namespace == project_name
}

url_is_public {
    data.exemptions.public[_].method == http_request.method
    glob.match(trim(data.exemptions.public[_].endpoint, "/"), ["/"], concat("/", input.parsed_path))
}

url_is_exempted {
    data.exemptions.global[_].method == http_request.method
    glob.match(trim(data.exemptions.global[_].endpoint, "/"), ["/"], concat("/", input.parsed_path))
}

url_is_exempted {
    data.exemptions.namespaced[_].method == http_request.method
    glob.match(trim(data.exemptions.namespaced[_].endpoint, "/"), ["/"], concat("/", input.parsed_path))
    user_projects[_] == project_name
}

project_name := pn {
    pn := input.parsed_query.projectName[0]
}

# get all projects which are visible by current user
user_projects[project] {
    some i
    data.bindings.role_bindings[i].user == claims.name
    project := data.bindings.role_bindings[i].bindings[_].namespace
}

# get all projects which are visible by all users (the user name is "*")
user_projects[project] {
    some i
    data.bindings.role_bindings[i].user == "*"
    project := data.bindings.role_bindings[i].bindings[_].namespace
}

# all projects which are allowed by current user
user_allowed_projects[project] {
    some project
    user_projects[project]
    not user_is_admin
    allow with project_name as project
}

# if user is system admin, return all projects
user_allowed_projects[project] {
    project := "*"
    user_is_admin
}

# all projects which are visible by current user
user_visible_projects[project] {
    some project
    user_projects[project]
    not user_is_admin
}

# if user is system admin, return all projects
user_visible_projects[project] {
    project := "*"
    user_is_admin
}


all_roles[role_ref] {
    some i
    data.bindings.role_bindings[i].user == claims.name
    role_ref := data.bindings.role_bindings[i].bindings[j].role_refs[_]
}

# only roles under the given project are allowed
allowed_roles[role_ref] {
    some i
    data.bindings.role_bindings[i].user == claims.name
    data.bindings.role_bindings[i].bindings[j].namespace == project_name
    role_ref := data.bindings.role_bindings[i].bindings[j].role_refs[_]
}

# if the proejct is visible by all users (the user name is "*"), the bound roles are also allowed
allowed_roles[role_ref] {
    some i
    data.bindings.role_bindings[i].user == "*"
    project := data.bindings.role_bindings[i].bindings[_].namespace == project_name
    role_ref := data.bindings.role_bindings[i].bindings[j].role_refs[_]
}

user_is_granted[grant] {
    some role_ref
    allowed_roles[role_ref]

    some i
    data.roles.roles[i].name == role_ref.name
    data.roles.roles[i].namespace == role_ref.namespace
    grant := data.roles.roles[i].rules[_]
}

claims := payload {
	# TODO: Verify the signature on the Bearer token. The certificate can be
	# hardcoded into the policy, and it could also be loaded via data or
	# an environment variable. Environment variables can be accessed using
	# the `opa.runtime()` built-in function.
	# io.jwt.verify_rs256(bearer_token, certificate)

	# This statement invokes the built-in function `io.jwt.decode` passing the
	# parsed bearer_token as a parameter. The `io.jwt.decode` function returns an
	# array:
	#
	#	[header, payload, signature]
	#
	# In Rego, you can pattern match values using the `=` and `:=` operators. This
	# example pattern matches on the result to obtain the JWT payload.
	[_, payload, _] := io.jwt.decode(bearer_token)
}

bearer_token := t {
	# Bearer tokens are contained inside of the HTTP Authorization header. This rule
	# parses the header and extracts the Bearer token value. If no Bearer token is
	# provided, the `bearer_token` value is undefined.
	v := http_request.headers.authorization
	startswith(v, "Bearer ")
	t := substring(v, count("Bearer "), -1)
}