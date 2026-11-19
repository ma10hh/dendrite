# Sourced helper: performs the OAuth authorization code flow against MAS for user $1 / password $2.
# Sets ACCESS, REFRESH, DEVICE, CID.
mas_login() {
  ACCESS=""; REFRESH=""
  local user=$1 pass=$2
  local jar; jar=$(mktemp); MAS_JAR=$jar
  local reg
  reg=$(curl -s -X POST "$MAS/oauth2/registration" -H 'Content-Type: application/json' -d '{"client_name":"Lab client","client_uri":"https://lab.example/","application_type":"native","redirect_uris":["http://127.0.0.1:9999/callback"],"token_endpoint_auth_method":"none","response_types":["code"],"grant_types":["authorization_code","refresh_token"]}')
  CID=$(echo "$reg" | jq -r .client_id)
  local verifier challenge
  verifier=$(openssl rand -hex 32)
  challenge=$(printf %s "$verifier" | openssl dgst -sha256 -binary | openssl base64 -A | tr '+/' '-_' | tr -d '=')
  DEVICE=LABDEV$(openssl rand -hex 4 | tr a-f A-F)
  local url="$MAS/authorize?response_type=code&client_id=$CID&redirect_uri=http%3A%2F%2F127.0.0.1%3A9999%2Fcallback&scope=openid%20urn%3Amatrix%3Aclient%3Aapi%3A*%20urn%3Amatrix%3Aclient%3Adevice%3A$DEVICE&state=st4te&code_challenge=$challenge&code_challenge_method=S256&response_mode=query"
  local next="$url" step body loc code
  for step in 1 2 3 4 5 6 7 8; do
    local hdr; hdr=$(mktemp)
    body=$(curl -s -D "$hdr" -c "$jar" -b "$jar" "$next")
    loc=$(grep -i '^location:' "$hdr" | tr -d '\r' | cut -d' ' -f2-)
    if [ -n "$loc" ]; then
      case "$loc" in
        http://127.0.0.1:9999/callback*) code=$(echo "$loc" | sed -n 's/.*[?&]code=\([^&#]*\).*/\1/p'); break ;;
        /*) next="$MAS$loc" ;;
        *) next="$loc" ;;
      esac
      continue
    fi
    local csrf; csrf=$(echo "$body" | grep -o 'name="csrf" value="[^"]*"' | head -1 | sed 's/.*value="\(.*\)"/\1/')
    if echo "$body" | grep -q 'name="username"'; then
      body=$(curl -s -D "$hdr" -c "$jar" -b "$jar" -X POST "$next" --data-urlencode "csrf=$csrf" --data-urlencode "username=$user" --data-urlencode "password=$pass")
    else
      body=$(curl -s -D "$hdr" -c "$jar" -b "$jar" -X POST "$next" --data-urlencode "csrf=$csrf")
    fi
    loc=$(grep -i '^location:' "$hdr" | tr -d '\r' | cut -d' ' -f2-)
    case "$loc" in
      http://127.0.0.1:9999/callback*) code=$(echo "$loc" | sed -n 's/.*[?&]code=\([^&#]*\).*/\1/p'); break ;;
      /*) next="$MAS$loc" ;;
      "") echo "login flow stuck at $next"; echo "$body" | sed 's/<[^>]*>//g' | grep -v '^\s*$' | head -8; return 1 ;;
      *) next="$loc" ;;
    esac
  done
  [ -n "${code:-}" ] || { echo "no authorization code"; return 1; }
  local tok
  tok=$(curl -s -X POST "$MAS/oauth2/token" -d grant_type=authorization_code -d "code=$code" -d client_id="$CID" --data-urlencode "redirect_uri=http://127.0.0.1:9999/callback" -d "code_verifier=$verifier")
  ACCESS=$(echo "$tok" | jq -r .access_token)
  REFRESH=$(echo "$tok" | jq -r .refresh_token)
  TOKEN_JSON=$tok
  [ "$ACCESS" != null ] || { echo "token request failed: $tok"; return 1; }
}
