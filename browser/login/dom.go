package login

// Only fixed, trusted functions run in the retained page's isolated world.
// They return static states or booleans, never field values or page content.
const loginDOM = `
function visible(el) {
  for(let n=0;el&&el.nodeType===1;el=el.parentElement) {
    if(++n>64||el.hidden||el.getAttribute('aria-hidden')==='true') return false;
    const s=getComputedStyle(el);
    if(s.display==='none'||s.visibility==='hidden'||s.visibility==='collapse'||s.opacity==='0') return false;
  }
  return true;
}
function visibleText(root) {
  const stack=[root],parts=[];let visits=0,size=0;
  while(stack.length&&visits++<5000&&size<16000) {
    const el=stack.pop();
    if(el.nodeType===3) { const s=(el.nodeValue||'').slice(0,16000-size).replace(/\s+/g,' ').trim();if(s){parts.push(s);size+=s.length+1;} }
    else if(el.nodeType===1&&visible(el)&&!/^(SCRIPT|STYLE|NOSCRIPT|TEMPLATE|INPUT|TEXTAREA|SELECT|OPTION)$/.test(el.tagName)) {
      for(let i=Math.min(el.childNodes.length,200)-1;i>=0&&stack.length<6000;i--) stack.push(el.childNodes[i]);
    }
  }
  return parts.join(' ');
}
function state(status,reason,submitted=false) { return {status,reason,submitted}; }
function permitted(raw,origin) {
  try { const u=new URL(raw,document.baseURI);return u.origin===origin&&(u.protocol==='https:'||u.protocol==='http:')&&!u.username&&!u.password&&!u.hash; } catch { return false; }
}
function attributes(el) { return (el.id||'')+' '+(el.getAttribute('name')||'')+' '+(el.getAttribute('autocomplete')||'')+' '+(el.getAttribute('class')||''); }
function codeMarker(el) { return /(?:^|[ _-])(?:otp|totp|mfa|2fa|code)(?:$|[ _-])|one[-_ ]?time[-_ ]?code|verification[-_ ]?code|authentication[-_ ]?code/i.test(attributes(el)); }
function editable(el) {
  if(el.disabled||el.readOnly) return false;
  return el.tagName==='INPUT'&&!/^(hidden|submit|button|reset|image)$/.test((el.type||'').toLowerCase())||el.tagName==='TEXTAREA'||el.tagName==='SELECT'||el.isContentEditable||/^(true|plaintext-only)$/i.test(el.getAttribute('contenteditable')||'');
}
function contract(form,submit,origin,fields) {
  if((form.getAttribute('method')||'get').toLowerCase()!=='post') return state('unsupported_form','post_form_required');
  if(['formaction','formmethod','formtarget','formenctype','formnovalidate'].some(name=>submit.hasAttribute(name))) return state('unsupported_form','submit_override');
  const baseTarget=document.querySelector('base[target]')?.getAttribute('target')||'', destination=form.getAttribute('target')||baseTarget;
  if(destination&&destination.toLowerCase()!=='_self') return state('unsupported_form','separate_target');
  const action=form.getAttribute('action')||location.href;
  if(!permitted(action,origin)) return state('unsupported_form','unsafe_form_destination');
  return JSON.stringify([location.href,new URL(action,document.baseURI).href,destination,form.getAttribute('method'),form.getAttribute('enctype'),form.getAttribute('accept-charset'),form.hasAttribute('novalidate'),...fields.map(el=>['type','name','id','autocomplete','pattern','inputmode','min','max','minlength','maxlength'].map(name=>el.getAttribute(name)))]);
}
function inspect(origin) {
  const stack=[document.documentElement],forms=[],editables=[];let visits=0,challenge=false,passwordPresent=false,blocked='';
  while(stack.length) {
    if(++visits>5000) return state('unsupported_form','page_too_complex');
    const el=stack.pop();if(!el||!visible(el)) continue;
    const tag=el.tagName;
    challenge=challenge||codeMarker(el);
    passwordPresent=passwordPresent||tag==='INPUT'&&(el.type||'').toLowerCase()==='password';
    if(/captcha|turnstile/i.test(attributes(el))) {challenge=true;blocked='challenge_detected';}
    if(tag==='IFRAME'||tag==='FRAME') blocked=blocked||'embedded_form';
    if(el.shadowRoot) blocked=blocked||'shadow_form';
    if(editable(el)) editables.push(el);
    if(tag==='FORM') forms.push(el);
    if(el.children.length>200) return state('unsupported_form','page_too_complex');
    for(let i=el.children.length-1;i>=0;i--) stack.push(el.children[i]);
  }
  if(blocked) return state(challenge?'requires_challenge':'unsupported_form',blocked);
  if(challenge) {
    if(passwordPresent) return state('requires_challenge','unsupported_challenge_form');
    if(forms.length!==1) return state('requires_challenge','challenge_form_required');
    const form=forms[0],codes=[],submits=[];
    if(form.elements.length>128) return state('requires_challenge','page_too_complex');
    for(const el of form.elements) {
      if(!visible(el)||el.disabled||el.readOnly) continue;
      if(!form.contains(el)) return state('requires_challenge','external_form_control');
      const tag=el.tagName,type=(el.type||'').toLowerCase();
      if(tag==='INPUT'&&type==='image') return state('requires_challenge','unsupported_challenge_form');
      if(tag==='INPUT'&&/^(text|tel|number)$/.test(type)&&codeMarker(el)) codes.push(el);
      if((tag==='BUTTON'||tag==='INPUT')&&type==='submit') submits.push(el);
    }
    if(codes.length!==1||submits.length!==1||editables.length!==1||editables[0]!==codes[0]) return state('requires_challenge','unsupported_challenge_form');
    const code=codes[0],submit=submits[0],signature=contract(form,submit,origin,[code,submit]);
    if(typeof signature!=='string') return state('requires_challenge',signature.reason);
    return {form,code,submit,signature};
  }
  const candidates=[];let usernameFirst=false;
  for(const form of forms) {
    if(form.elements.length>128) return state('unsupported_form','page_too_complex');
    const passwords=[],usernames=[],submits=[];let newPassword=false;
    for(const el of form.elements) {
      if(!visible(el)||el.disabled||el.readOnly) continue;
      if(!form.contains(el)) return state('unsupported_form','external_form_control');
      const tag=el.tagName,type=(el.type||'').toLowerCase(),autocomplete=(el.getAttribute('autocomplete')||'').toLowerCase();
      if(tag==='INPUT'&&type==='password') {passwords.push(el);newPassword=newPassword||autocomplete.split(/\s+/).includes('new-password');}
      if(tag==='INPUT'&&(type==='text'||type==='email')) usernames.push(el);
      if((tag==='BUTTON'||tag==='INPUT')&&type==='submit') submits.push(el);
    }
    if(!passwords.length) {usernameFirst=usernameFirst||usernames.some(el=>/username|email|login|account/i.test((el.name||'')+' '+(el.id||'')+' '+(el.getAttribute('autocomplete')||'')));continue;}
    if(newPassword) return state('unsupported_form','new_password_form');
    if(passwords.length===1&&!usernames.length) return state('unsupported_form','password_only_form');
    if(passwords.length!==1||usernames.length!==1||submits.length!==1) return state('unsupported_form','ambiguous_credentials');
    const username=usernames[0],password=passwords[0],submit=submits[0];
    const signature=contract(form,submit,origin,[username,password,submit]);
    if(typeof signature!=='string') return signature;
    candidates.push({form,username,password,submit,signature});
  }
  if(candidates.length!==1) return state('unsupported_form',candidates.length?'ambiguous_forms':usernameFirst?'username_first_form':'password_form_required');
  return candidates[0];
}
`

const inspectLoginDOM = `
const found=inspect(args);
if(found.status) return found;
if(found.code) {
  globalThis.__commanderLoginChallenge=found;
  return state('supported_challenge','one_time_code_form');
}
globalThis.__commanderLogin=found;
return state('supported','password_form');
`

const submitLoginDOM = `
const saved=globalThis.__commanderLogin,found=inspect(args.origin);
if(found.status) return found;
if(found.code) return state('requires_challenge','challenge_detected');
if(!saved||found.form!==saved.form||found.username!==saved.username||found.password!==saved.password||found.submit!==saved.submit||found.signature!==saved.signature) return state('unsupported_form','form_changed');
found.username.value=args.username;found.password.value=args.password;
for(const el of [found.username,found.password]) {el.dispatchEvent(new Event('input',{bubbles:true}));el.dispatchEvent(new Event('change',{bubbles:true}));}
const checked=inspect(args.origin);
if(checked.status) return checked.status==='requires_challenge' ? state(checked.status,checked.reason,true) : state('submission_uncertain','form_changed_after_fill',true);
if(checked.code) return state('requires_challenge','challenge_detected',true);
if(checked.form!==saved.form||checked.username!==saved.username||checked.password!==saved.password||checked.submit!==saved.submit||checked.signature!==saved.signature) return state('submission_uncertain','form_changed_after_fill',true);
found.submit.click();
return state('submitted','submission_sent',true);
`

const submitChallengeDOM = `
const saved=globalThis.__commanderLoginChallenge,found=inspect(args.origin);
if(found.status) return state('requires_challenge',found.reason);
if(!found.code||!saved||found.form!==saved.form||found.code!==saved.code||found.submit!==saved.submit||found.signature!==saved.signature) return state('requires_challenge','challenge_form_changed');
found.code.value=args.code;
found.code.dispatchEvent(new Event('input',{bubbles:true}));
found.code.dispatchEvent(new Event('change',{bubbles:true}));
const checked=inspect(args.origin);
if(checked.status||!checked.code||checked.form!==saved.form||checked.code!==saved.code||checked.submit!==saved.submit||checked.signature!==saved.signature) return state('submission_uncertain','challenge_form_changed_after_fill',true);
found.submit.click();
return state('submitted','challenge_submission_sent',true);
`
