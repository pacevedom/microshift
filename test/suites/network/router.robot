*** Settings ***
Documentation       Router tests

Resource            ../../resources/common.resource
Resource            ../../resources/oc.resource
Resource            ../../resources/ostree-health.resource
Resource            ../../resources/microshift-network.resource
Resource            ../../resources/microshift-config.resource

Suite Setup         Setup Suite With Namespace
Suite Teardown      Teardown Suite With Namespace

Test Tags           slow


*** Variables ***
${ALTERNATIVE_HTTP_PORT}    8000
${ALTERNATIVE_HTTPS_PORT}    9000
${HOSTNAME}                 hello-microshift.cluster.local
${ROUTER_DISABLED}          SEPARATOR=\n
...                     ---
...                     ingress:
...                     \ \ status: Disabled
${ROUTER_CUSTOM_PORTS}    SEPARATOR=\n
...                     ---
...                     ingress:
...                     \ \ status: Enabled
...                     \ \ ports:
...                     \ \ \ \ http: ${ALTERNATIVE_HTTP_PORT}
...                     \ \ \ \ https: ${ALTERNATIVE_HTTPS_PORT}

*** Test Cases ***
Router Default Configuration
    [Documentation]    Check default configuration, router enabled and standard ports and expose.
    [Setup]    Run Keywords
    ...    Save Default MicroShift Config
    ...    Clear MicroShift Config
    ...    Restart MicroShift
    ...    Create Hello MicroShift Pod
    ...    Expose Hello MicroShift Service Via Route
    ...    Restart Router

    Wait Until Keyword Succeeds    10x    6s
    ...    Access Hello Microshift    ${HTTP_PORT}

    [Teardown]    Run Keywords
    ...    Delete Hello MicroShift Route
    ...    Delete Hello MicroShift Pod And Service
    ...    Wait For Service Deletion With Timeout
    ...    Restore Default MicroShift Config
    ...    Restart MicroShift

Router Disable
    [Documentation]    Disable the router and check the namespace does not exist.
    [Setup]    Run Keywords
    ...    Save Default MicroShift Config
    ...    Disable Router

    Run With Kubeconfig    oc wait --for=delete namespace/openshift-ingress --timeout=60s

    [Teardown]    Run Keywords
    ...    Restore Default MicroShift Config
    ...    Restart MicroShift

Router Listen Custom Ports
    [Documentation]    Change default listening ports in the router and check they work.
    [Setup]    Run Keywords
    ...    Save Default MicroShift Config
    ...    Change Listening Ports
    ...    Create Hello MicroShift Pod
    ...    Expose Hello MicroShift Service Via Route
    ...    Restart Router

    Wait Until Keyword Succeeds    10x    6s
    ...    Access Hello Microshift    ${ALTERNATIVE_HTTP_PORT}

    [Teardown]    Run Keywords
    ...    Delete Hello MicroShift Route
    ...    Delete Hello MicroShift Pod And Service
    ...    Wait For Service Deletion With Timeout
    ...    Restore Default MicroShift Config
    ...    Restart MicroShift

*** Keywords ***
Restart Router
    [Documentation]    Restart the router and wait for readiness again. The router is sensitive to apiserver
    ...    downtime and might need a restart (after the apiserver is ready) to resync all the routes.
    Run With Kubeconfig    oc rollout restart deployment router-default -n openshift-ingress
    Named Deployment Should Be Available    router-default    openshift-ingress    5m

Expose Hello MicroShift Service Via Route
    [Documentation]    Expose the "hello microshift" application through the Route
    Oc Expose    pod hello-microshift -n ${NAMESPACE}
    Oc Expose    svc hello-microshift --hostname hello-microshift.cluster.local -n ${NAMESPACE}

Delete Hello MicroShift Route
    [Documentation]    Delete route for cleanup.
    Oc Delete    route/hello-microshift -n ${NAMESPACE}

Wait For Service Deletion With Timeout
    [Documentation]    Polls for service and endpoint by "app=hello-microshift" label. Fails if timeout
    ...    expires. This check is unique to this test suite because each test here reuses the same namespace. Since
    ...    the tests reuse the service name, a small race window exists between the teardown of one test and the setup
    ...    of the next. This produces flakey failures when the service or endpoint names collide.
    Wait Until Keyword Succeeds    30s    1s
    ...    Network APIs With Test Label Are Gone

Network APIs With Test Label Are Gone
    [Documentation]    Check for service and endpoint by "app=hello-microshift" label. Succeeds if response matches
    ...    "No resources found in <namespace> namespace." Fail if not.
    ${match_string}=    Catenate    No resources found in    ${NAMESPACE}    namespace.
    ${match_string}=    Remove String    ${match_string}    "
    ${response}=    Run With Kubeconfig    oc get svc,ep -l app\=hello-microshift -n ${NAMESPACE}
    Should Be Equal As Strings    ${match_string}    ${response}    strip_spaces=True

Disable Router
    [Documentation]    Disable router
    Setup With Custom Config    ${ROUTER_DISABLED}

Change Listening Ports
    [Documentation]    Enable router and change the default listening ports
    Setup With Custom Config    ${ROUTER_CUSTOM_PORTS}

Setup With Custom Config
    [Documentation]    Install a custom config and restart MicroShift
    [Arguments]    ${config_content}
    ${merged}=    Extend MicroShift Config    ${config_content}
    Upload MicroShift Config    ${merged}
    Restart MicroShift