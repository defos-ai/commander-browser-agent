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
function inspect(origin) {
  const stack=[document.documentElement],forms=[];let visits=0;
  while(stack.length) {
    if(++visits>5000) return state('unsupported_form','page_too_complex');
    const el=stack.pop();if(!el||!visible(el)) continue;
    const tag=el.tagName, attributes=(el.id||'')+' '+(el.getAttribute('name')||'')+' '+(el.getAttribute('autocomplete')||'')+' '+(el.getAttribute('class')||'');
    if(/captcha|turnstile|(?:^|[ _-])(?:otp|totp|mfa|2fa|code)(?:$|[ _-])|one[-_ ]?time[-_ ]?code|verification[-_ ]?code|authentication[-_ ]?code/i.test(attributes)) return state('requires_challenge','challenge_detected');
    if(tag==='IFRAME'||tag==='FRAME') return state('unsupported_form','embedded_form');
    if(el.shadowRoot) return state('unsupported_form','shadow_form');
    if(tag==='FORM') forms.push(el);
    if(el.children.length>200) return state('unsupported_form','page_too_complex');
    for(let i=el.children.length-1;i>=0;i--) stack.push(el.children[i]);
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
    if((form.getAttribute('method')||'get').toLowerCase()!=='post') return state('unsupported_form','post_form_required');
    if(['formaction','formmethod','formtarget','formenctype','formnovalidate'].some(name=>submit.hasAttribute(name))) return state('unsupported_form','submit_override');
    const baseTarget=document.querySelector('base[target]')?.getAttribute('target')||'', destination=form.getAttribute('target')||baseTarget;
    if(destination&&destination.toLowerCase()!=='_self') return state('unsupported_form','separate_target');
    if(!permitted(form.getAttribute('action')||location.href,origin)) return state('unsupported_form','unsafe_form_destination');
    candidates.push({form,username,password,submit});
  }
  if(candidates.length!==1) return state('unsupported_form',candidates.length?'ambiguous_forms':usernameFirst?'username_first_form':'password_form_required');
  return candidates[0];
}
`

const inspectLoginDOM = `
const found=inspect(args);
if(found.status) return found;
globalThis.__commanderLogin=found;
return state('supported','password_form');
`

const submitLoginDOM = `
const saved=globalThis.__commanderLogin,found=inspect(args.origin);
if(found.status) return found;
if(!saved||found.form!==saved.form||found.username!==saved.username||found.password!==saved.password||found.submit!==saved.submit) return state('unsupported_form','form_changed');
found.username.value=args.username;found.password.value=args.password;
for(const el of [found.username,found.password]) {el.dispatchEvent(new Event('input',{bubbles:true}));el.dispatchEvent(new Event('change',{bubbles:true}));}
const checked=inspect(args.origin);
if(checked.status) return checked.status==='requires_challenge' ? state(checked.status,checked.reason,true) : state('submission_uncertain','form_changed_after_fill',true);
if(checked.form!==saved.form||checked.username!==saved.username||checked.password!==saved.password||checked.submit!==saved.submit) return state('submission_uncertain','form_changed_after_fill',true);
found.submit.click();
return state('submitted','submission_sent',true);
`
